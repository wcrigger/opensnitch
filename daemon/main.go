package main

import (
	"flag"
	"io/ioutil"
	golog "log"
	"os"
	"os/signal"
	"syscall"

	"github.com/evilsocket/opensnitch/daemon/conman"
	"github.com/evilsocket/opensnitch/daemon/core"
	"github.com/evilsocket/opensnitch/daemon/dns"
	"github.com/evilsocket/opensnitch/daemon/firewall"
	"github.com/evilsocket/opensnitch/daemon/log"
	"github.com/evilsocket/opensnitch/daemon/rule"
	"github.com/evilsocket/opensnitch/daemon/statistics"
	"github.com/evilsocket/opensnitch/daemon/ui"

	"github.com/evilsocket/go-netfilter-queue"
)

var (
	logFile   = ""
	rulesPath = "rules"
	queueNum  = 0
	workers   = 16
	debug     = false

	uiSocket = "unix:///tmp/osui.sock"
	uiClient = (*ui.Client)(nil)

	err     = (error)(nil)
	rules   = rule.NewLoader()
	stats   = statistics.New()
	queue   = (*netfilter.NFQueue)(nil)
	pktChan = (<-chan netfilter.NFPacket)(nil)
	wrkChan = (chan netfilter.NFPacket)(nil)
	sigChan = (chan os.Signal)(nil)
)

func init() {
	flag.StringVar(&uiSocket, "ui-socket", uiSocket, "Path the UI gRPC service listener (https://github.com/grpc/grpc/blob/master/doc/naming.md).")
	flag.StringVar(&rulesPath, "rules-path", rulesPath, "Path to load JSON rules from.")
	flag.IntVar(&queueNum, "queue-num", queueNum, "Netfilter queue number.")
	flag.IntVar(&workers, "workers", workers, "Number of concurrent workers.")

	flag.StringVar(&logFile, "log-file", logFile, "Write logs to this file instead of the standard output.")
	flag.BoolVar(&debug, "debug", debug, "Enable debug logs.")
}

func setupSignals() {
	sigChan = make(chan os.Signal, 1)
	signal.Notify(sigChan,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	go func() {
		sig := <-sigChan
		log.Raw("\n")
		log.Important("Got signal: %v", sig)
		doCleanup()
		os.Exit(0)
	}()
}

func worker(id int) {
	log.Debug("Worker #%d started.", id)
	for true {
		select {
		case pkt := <-wrkChan:
			onPacket(pkt)
		}
	}
}

func setupWorkers() {
	log.Debug("Starting %d workers ...", workers)
	// setup the workers
	wrkChan = make(chan netfilter.NFPacket)
	for i := 0; i < workers; i++ {
		go worker(i)
	}
}

func doCleanup() {
	log.Info("Cleaning up ...")
	firewall.QueueDNSResponses(false, queueNum)
	firewall.QueueConnections(false, queueNum)
	firewall.RejectMarked(false)
}

func onPacket(packet netfilter.NFPacket) {
	// DNS response, just parse, track and accept.
	if dns.TrackAnswers(packet.Packet) == true {
		packet.SetVerdict(netfilter.NF_ACCEPT)
		stats.OnDNSResponse()
		return
	}

	// Parse the connection state
	con := conman.Parse(packet)
	if con == nil {
		packet.SetVerdict(netfilter.NF_ACCEPT)
		stats.OnIgnored()
		return
	}

	// search a match in preloaded rules
	connected := false
	missed := false
	r := rules.FindFirstMatch(con)
	if r == nil {
		missed = true
		// no rule matched, send a request to the
		// UI client if connected and running
		r, connected = uiClient.Ask(con)
		if connected {
			ok := false
			pers := ""
			action := string(r.Action)
			if r.Action == rule.Allow {
				action = log.Green(action)
			} else {
				action = log.Red(action)
			}

			// check if and how the rule needs to be saved
			if r.Duration == rule.Restart {
				pers = "Added"
				// add to the rules but do not save to disk
				if err := rules.Add(r, false); err != nil {
					log.Error("Error while adding rule: %s", err)
				} else {
					ok = true
				}
			} else if r.Duration == rule.Always {
				pers = "Saved"
				// add to the loaded rules and persist on disk
				if err := rules.Add(r, true); err != nil {
					log.Error("Error while saving rule: %s", err)
				} else {
					ok = true
				}
			}

			if ok {
				log.Important("%s new rule: %s if %s", pers, action, r.Operator.String())
			}
		}
	}

	stats.OnConnectionEvent(con, r, missed)

	if r.Action == rule.Allow {
		packet.SetVerdict(netfilter.NF_ACCEPT)

		ruleName := log.Green(r.Name)
		if r.Operator.Operand == rule.OpTrue {
			ruleName = log.Dim(r.Name)
		}
		log.Debug("%s %s -> %s:%d (%s)", log.Bold(log.Green("✔")), log.Bold(con.Process.Path), log.Bold(con.To()), con.DstPort, ruleName)
		return
	}

	packet.SetVerdict(netfilter.NF_DROP)

	log.Warning("%s %s -> %s:%d (%s)", log.Bold(log.Red("✘")), log.Bold(con.Process.Path), log.Bold(con.To()), con.DstPort, log.Red(r.Name))
}

func main() {
	golog.SetOutput(ioutil.Discard)
	flag.Parse()

	if debug {
		log.MinLevel = log.DEBUG
	} else {
		log.MinLevel = log.INFO
	}

	if logFile != "" {
		if log.Output, err = os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err != nil {
			panic(err)
		}
	}

	log.Important("Starting %s v%s", core.Name, core.Version)

	rulesPath, err := core.ExpandPath(rulesPath)
	if err != nil {
		log.Fatal("%s", err)
	}

	setupSignals()

	log.Info("Loading rules from %s ...", rulesPath)
	if err := rules.Load(rulesPath); err != nil {
		log.Fatal("%s", err)
	}
	uiClient = ui.NewClient(uiSocket, stats)

	// prepare the queue
	setupWorkers()
	queue, err := netfilter.NewNFQueue(uint16(queueNum), 4096, netfilter.NF_DEFAULT_PACKET_SIZE)
	if err != nil {
		log.Fatal("Error while creating queue #%d: %s", queueNum, err)
	}
	pktChan = queue.GetPackets()

	// queue is ready, run firewall rules
	if err = firewall.QueueDNSResponses(true, queueNum); err != nil {
		log.Fatal("Error while running DNS firewall rule: %s", err)
	} else if err = firewall.QueueConnections(true, queueNum); err != nil {
		log.Fatal("Error while running conntrack firewall rule: %s", err)
	} else if err = firewall.RejectMarked(true); err != nil {
		log.Fatal("Error while running reject firewall rule: %s", err)
	}

	log.Info("Running on netfilter queue #%d ...", queueNum)
	for true {
		select {
		case pkt := <-pktChan:
			wrkChan <- pkt
		}
	}
}

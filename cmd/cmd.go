// Copyright (c) 2022, R.I. Pienaar and the Choria Project contributors
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	pphttp "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/choria-io/stream-replicator/advisor"
	"github.com/choria-io/stream-replicator/idtrack"
	"github.com/choria-io/tokens"
	"github.com/nats-io/jsm.go"
	"github.com/nats-io/jsm.go/natscontext"
	"github.com/nats-io/nats.go"

	"github.com/choria-io/fisk"
	"github.com/choria-io/stream-replicator/config"
	"github.com/choria-io/stream-replicator/replicator"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

var (
	version = "development"
	sha     = ""
)

type cmd struct {
	cfgile string
	debug  bool

	findStream       string
	findValue        string
	findSince        time.Duration
	findFollow       bool
	nCtx             string
	stateSource      string
	stateValue       *regexp.Regexp
	stateValuesOnly  bool
	stateAdvised     bool
	stateSince       time.Duration
	json             bool
	choriaToken      string
	choriaSeed       string
	choriaCollective string

	mu  sync.Mutex
	log *logrus.Entry
}

func Run() {
	c := &cmd{}

	help := fmt.Sprintf("Choria Stream Replicator version %s", version)
	if len(sha) > 8 {
		help = fmt.Sprintf("Choria Stream Replicator version %s (%s)", version, []byte(sha)[0:8])
	}

	app := fisk.New("stream-replicator", help)
	app.Author("R.I.Pienaar <rip@devco.net>")
	app.Version(version)
	app.Flag("debug", "Enables debug logging").UnNegatableBoolVar(&c.debug)

	repl := app.Command("replicate", "Starts the Stream Replicator process").Default().Action(c.replicateAction)
	repl.Flag("config", "Configuration file").Required().ExistingFileVar(&c.cfgile)

	admin := app.Command("admin", "Interact with stream advisories and tracking state")
	admFind := admin.Command("advisories", "Audit advisories for a specific node").Alias("adv").Action(c.findAction)
	admFind.Arg("stream", "The name of the stream holding advisories").Required().StringVar(&c.findStream)
	admFind.Arg("value", "The value to search for in the advisories").StringVar(&c.findValue)
	admFind.Flag("since", "Finds messages since a certain age expressed as a duration like 5m").Default("1m").DurationVar(&c.findSince)
	admFind.Flag("follow", "Follow when end was reached rather than terminating").Short('f').BoolVar(&c.findFollow)
	admFind.Flag("context", "The NATS context to use for the connection").StringVar(&c.nCtx)
	admFind.Flag("choria-seed", "The seed file to connect to Choria Brokers with").ExistingFileVar(&c.choriaSeed)
	admFind.Flag("choria-token", "The JWT token file to connect to Choria Brokers with").ExistingFileVar(&c.choriaToken)
	admFind.Flag("choria-collective", "The Choria collective you will be connecting to").Default("choria").StringVar(&c.choriaCollective)

	admState := admin.Commandf("state", "Search state files").Action(c.findState)
	admState.Arg("dir", "Directory where state files are kept").Required().StringVar(&c.stateSource)
	admState.Flag("since", "List only entries seen since a certain duration ago").DurationVar(&c.stateSince)
	admState.Flag("advised", "Include entries that are in warning state").Default("true").BoolVar(&c.stateAdvised)
	admState.Flag("value", "A regular expression value to search for").RegexpVar(&c.stateValue)
	admState.Flag("values", "List only values rather than full entries").BoolVar(&c.stateValuesOnly)
	admState.Flag("json", "Render JSON values").BoolVar(&c.json)

	admGossip := admin.Commandf("gossip", "View the synchronization traffic").Action(c.gossipAction)
	admGossip.Flag("json", "Render JSON values").BoolVar(&c.json)
	admGossip.Flag("context", "The NATS context to use for the connection").StringVar(&c.nCtx)
	admGossip.Flag("choria-seed", "The seed file to connect to Choria Brokers with").ExistingFileVar(&c.choriaSeed)
	admGossip.Flag("choria-token", "The JWT token file to connect to Choria Brokers with").ExistingFileVar(&c.choriaToken)
	admGossip.Flag("choria-collective", "The Choria collective you will be connecting to").Default("choria").StringVar(&c.choriaCollective)

	app.MustParseWithUsage(os.Args[1:])
}

func (c *cmd) findStateFiles(dir string) ([]string, error) {
	var paths []string

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		if filepath.Ext(path) != ".json" {
			return nil
		}

		paths = append(paths, path)

		return nil
	})

	return paths, err
}

func (c *cmd) findState(_ *fisk.ParseContext) error {
	var paths []string
	var err error

	if stat, err := os.Stat(c.stateSource); err == nil {
		if !stat.IsDir() {
			paths = append(paths, c.stateSource)
		}
	}

	if len(paths) == 0 {
		paths, err = c.findStateFiles(c.stateSource)
		if err != nil {
			return err
		}
	}

	var selected []*idtrack.Item

	for _, path := range paths {
		items := idtrack.Tracker{}
		sb, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		err = json.Unmarshal(sb, &items)
		if err != nil {
			return err
		}

		for v, item := range items.Items {
			item.Value = v

			if c.stateSince > 0 && time.Since(item.Seen) >= c.stateSince {
				continue
			}

			if !c.stateAdvised && item.Advised {
				continue
			}

			if c.stateValue != nil && !c.stateValue.MatchString(item.Value) {
				continue
			}

			selected = append(selected, item)
		}
	}

	if len(selected) == 0 {
		if c.stateValuesOnly {
			return nil
		}
		if c.json {
			fmt.Println("[]")
			return nil
		}

		fmt.Println("No items matched")
		return nil
	}

	// just values used later should we need to json dump
	var values []string

	// avoid duplicate names though in practice one would partition on fqdn so there wouldnt be dupes but worth doing anyway
	if c.stateValuesOnly {
		uniq := []*idtrack.Item{}
		seen := make(map[string]struct{})

		for _, item := range selected {
			if _, ok := seen[item.Value]; ok {
				continue
			}

			uniq = append(uniq, item)
			values = append(values, item.Value)
			seen[item.Value] = struct{}{}
		}

		selected = uniq
	}

	if c.json {
		v := any(selected)
		if c.stateValuesOnly {
			v = values
		}

		j, err := json.Marshal(v)
		if err != nil {
			return err
		}
		fmt.Println(string(j))
		return nil
	}

	for _, item := range selected {
		if c.stateValuesOnly {
			fmt.Println(item.Value)
			continue
		}

		fmt.Printf("           Value: %v\n", item.Value)
		if item.Seen.IsZero() {
			fmt.Printf("       Seen Time: never\n")
		} else {
			fmt.Printf("       Seen Time: %v (%v)\n", item.Seen, time.Since(item.Seen).Round(time.Second))
		}

		if item.Copied.IsZero() {
			fmt.Printf("     Copied Time: never\n")
		} else {
			fmt.Printf("     Copied Time: %v (%v)\n", item.Copied, time.Since(item.Copied).Round(time.Second))
		}
		fmt.Printf("    Payload Size: %v\n", item.Size)
		fmt.Printf("         Advised: %t\n", item.Advised)

		fmt.Println()
	}

	return nil
}

func (c *cmd) connect() (*nats.Conn, error) {
	var opts []nats.Option

	log := logrus.New().WithFields(logrus.Fields{"context": c.nCtx, "seed": c.choriaSeed, "jwt": c.choriaToken, "collective": c.choriaCollective})
	if c.debug {
		log.Logger.SetLevel(logrus.DebugLevel)
	}

	if c.choriaToken != "" && c.choriaSeed != "" {
		log.Debugf("Configuring Choria Connection")

		token, err := os.ReadFile(c.choriaToken)
		if err != nil {
			return nil, err
		}

		inbox, jwth, sigh, err := tokens.NatsConnectionHelpers(string(token), c.choriaCollective, c.choriaSeed, logrus.NewEntry(logrus.New()))
		if err != nil {
			return nil, fmt.Errorf("could not set up choria connection: %w", err)
		}

		opts = append(opts, nats.Token(string(token)))
		opts = append(opts, nats.CustomInboxPrefix(inbox))
		opts = append(opts, nats.UserJWT(jwth, sigh))
		opts = append(opts, nats.Secure(&tls.Config{InsecureSkipVerify: true}))
	}

	log.Debugf("Connecting to NATS server")
	return natscontext.Connect(c.nCtx, opts...)
}

func (c *cmd) gossipAction(_ *fisk.ParseContext) error {
	if c.nCtx == "" && natscontext.SelectedContext() == "" {
		return fmt.Errorf("a NATS context is required when a default context is not selected")
	}

	nc, err := c.connect()
	if err != nil {
		return err
	}

	prefix := "choria.stream-replicator.sync."
	sub, err := nc.SubscribeSync(fmt.Sprintf("%s>", prefix))
	if err != nil {
		return err
	}

	for {
		msg, err := sub.NextMsg(time.Minute)
		if err != nil {
			return err
		}

		if c.json {
			fmt.Println(string(msg.Data))
			continue
		}

		i := &idtrack.Item{}
		err = json.Unmarshal(msg.Data, i)
		if err != nil {
			c.log.Errorf("Could not process sync item: %v", err)
			continue
		}

		fmt.Printf("[%s] size: %.0f advised: %t: copied: %s %s\n", strings.TrimPrefix(msg.Subject, prefix), i.Size, i.Advised, time.Since(i.Copied).Round(time.Millisecond), i.Value)
	}
}

func (c *cmd) findAction(_ *fisk.ParseContext) error {
	if c.nCtx == "" && natscontext.SelectedContext() == "" {
		return fmt.Errorf("a NATS context is required when a default context is not selected")
	}

	nc, err := c.connect()
	if err != nil {
		return err
	}

	mgr, err := jsm.New(nc)
	if err != nil {
		return err
	}

	sub, err := nc.SubscribeSync(nc.NewRespInbox())
	if err != nil {
		return err
	}

	opts := []jsm.ConsumerOption{jsm.DeliverySubject(sub.Subject), jsm.AcknowledgeExplicit(), jsm.MaxAckPending(1)}
	if c.findSince > 0 {
		opts = append(opts, jsm.StartAtTimeDelta(c.findSince))
	}

	_, err = mgr.NewConsumer(c.findStream, opts...)
	if err != nil {
		return err
	}

	cnt := 0

	for {
		msg, err := sub.NextMsg(time.Second)
		if err != nil {
			if cnt == 0 {
				return fmt.Errorf("did not find any messages for %q", c.findValue)
			} else {
				if c.findFollow {
					continue
				}
				return err
			}
		}

		meta, _ := jsm.ParseJSMsgMetadata(msg)

		if cnt == 0 && meta != nil {
			if c.findValue == "" {
				fmt.Printf("Searching for advisories\n\n")
			} else {
				fmt.Printf("Searching %d messages for advisories related to %v\n\n", meta.Pending()+1, c.findValue)
			}

		}

		msg.Ack()
		cnt++

		if len(msg.Data) == 0 {
			continue
		}

		adv := &advisor.AgeAdvisoryV2{}
		err = json.Unmarshal(msg.Data, adv)
		if err != nil {
			fmt.Printf("Could not process message %q: %v\n", msg.Data, err)
			continue
		}

		if c.findValue == "" || adv.Value == c.findValue {
			ts := time.Unix(adv.Timestamp, 0)
			seen := time.Unix(adv.Seen, 0)
			fmt.Printf("[%v] %7s %s seen %v earlier on %s\n", ts.Format("2006-01-02 15:04:05"), adv.Event, adv.Value, ts.Sub(seen), adv.Replicator)
		}

		if !c.findFollow && meta != nil && meta.Pending() == 0 {
			break
		}
	}

	return nil
}

func (c *cmd) replicateAction(_ *fisk.ParseContext) error {
	cfg, err := config.Load(c.cfgile)
	if err != nil {
		return err
	}

	c.log, err = c.configureLogging(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wg := &sync.WaitGroup{}

	go c.interruptHandler(ctx, cancel)

	go c.setupPrometheus(cfg.MonitorPort, cfg.Profiling)

	for _, s := range cfg.Streams {
		c.log.Debugf("Configuring stream %s", s.Name)
		stream, err := replicator.NewStream(s, cfg, c.log)
		if err != nil {
			return err
		}

		wg.Add(1)
		go func(s *config.Stream) {
			defer wg.Done()

			wg.Add(1)
			err = stream.Run(ctx, wg)
			if err != nil {
				c.log.Errorf("Could not start replicator for %s: %v", s.Name, err)
			}
		}(s)
	}

	wg.Wait()

	return nil
}

func (c *cmd) setupPrometheus(port int, profiling bool) {
	if port == 0 {
		c.log.Infof("Skipping Prometheus setup")
		return
	}

	c.log.Infof("Listening for /metrics on %d", port)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	if profiling {
		c.log.Warnf("Enabling live profiling on /debug/pprof")
		mux.HandleFunc("/debug/pprof/", pphttp.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pphttp.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pphttp.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pphttp.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pphttp.Trace)
	}

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	c.log.Fatal(server.ListenAndServe())
}

func (c *cmd) configureLogging(cfg *config.Config) (*logrus.Entry, error) {
	logger := logrus.New()

	if cfg.LogFile != "" {
		logger.SetFormatter(&logrus.JSONFormatter{})

		file, err := os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			return nil, err
		}

		logger.SetOutput(file)
	}

	switch cfg.LogLevel {
	case "debug":
		logger.SetLevel(logrus.DebugLevel)
	case "warn":
		logger.SetLevel(logrus.WarnLevel)
	default:
		logger.SetLevel(logrus.InfoLevel)
	}

	if c.debug {
		logger.SetLevel(logrus.DebugLevel)
		logger.Infof("Forcing debug logging due to CLI override")
	}

	return logrus.NewEntry(logger), nil
}

func (c *cmd) interruptHandler(ctx context.Context, cancel context.CancelFunc) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGQUIT {
				c.log.Warnf("Dumping internal state on signal %s", sig)
				c.dumpGoRoutines()
				continue
			}

			c.log.Warnf("Shutting down on signal %s", sig)
			cancel()
		case <-ctx.Done():
			return
		}
	}
}

func (c *cmd) dumpGoRoutines() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().UnixNano()
	pid := os.Getpid()

	tdoutname := filepath.Join(os.TempDir(), fmt.Sprintf("sr-threaddump-%d-%d.txt", pid, now))
	memoutname := filepath.Join(os.TempDir(), fmt.Sprintf("sr-memoryprofile-%d-%d.mprof", pid, now))

	buf := make([]byte, 1<<20)
	stacklen := runtime.Stack(buf, true)

	err := os.WriteFile(tdoutname, buf[:stacklen], 0644)
	if err != nil {
		c.log.Errorf("Could not produce thread dump: %s", err)
		return
	}

	c.log.Warnf("Produced thread dump to %s", tdoutname)

	mf, err := os.Create(memoutname)
	if err != nil {
		c.log.Errorf("Could not produce memory profile: %s", err)
		return
	}
	defer mf.Close()

	err = pprof.WriteHeapProfile(mf)
	if err != nil {
		c.log.Errorf("Could not produce memory profile: %s", err)
		return
	}

	c.log.Warnf("Produced memory profile to %s", memoutname)
}

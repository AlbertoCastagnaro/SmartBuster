// Command smartbuster is the CLI entry point: flag parsing and wiring for
// the engine, audit log, and result output.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/audit"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/corpus"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/daemon"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/output"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
	passiveseed "github.com/AlbertoCastagnaro/SmartBuster/internal/seed"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/wordlist"
)

const version = "smartbuster/0.1.0 (phase1)"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "scan":
		runScan(os.Args[2:])
	case "serve":
		runServe(os.Args[2:])
	case "resume":
		runResume(os.Args[2:])
	case "ruleset":
		runRuleset(os.Args[2:])
	case "corpus":
		runCorpus(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "smartbuster: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: smartbuster scan <target> [<target>...] [-w <wordlist>] [flags]")
	fmt.Fprintln(os.Stderr, "       smartbuster serve [--port <n>] [--open] [--bind <addr>]")
	fmt.Fprintln(os.Stderr, "       smartbuster resume <session-file.json> [flags]")
	fmt.Fprintln(os.Stderr, "       smartbuster ruleset update --repo <url> --commit <ref> [--dest <dir>]")
	fmt.Fprintln(os.Stderr, "       smartbuster corpus build --seclists <path> [--source-map <file>] [--out <db>]")
	fmt.Fprintln(os.Stderr, "       smartbuster corpus import <file> --tags <a,b,...> --type dir|file [--db <path>]")
}

// defaultCorpusDBPath mirrors defaultSystemRulesetDir's convention: a
// per-user config directory location `corpus build`/`corpus import` write
// to by default when --out/--db isn't given.
func defaultCorpusDBPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "smartbuster", "corpus.db")
}

func runCorpus(args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "build":
		runCorpusBuild(args[1:])
	case "import":
		runCorpusImport(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "smartbuster corpus: unknown subcommand %q\n", args[0])
		usage()
		os.Exit(2)
	}
}

func runCorpusBuild(args []string) {
	fs := flag.NewFlagSet("corpus build", flag.ExitOnError)
	seclistsPath := fs.String("seclists", "", "path to a SecLists checkout (required)")
	sourceMapPath := fs.String("source-map", "", "sourcemap.yaml; \"\" = bundled default (spec §3)")
	out := fs.String("out", defaultCorpusDBPath(), "corpus DB file to write")
	fs.Parse(args)

	if *seclistsPath == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: smartbuster corpus build --seclists <path> [--source-map <file>] [--out <db>]")
		os.Exit(2)
	}

	var smData []byte
	var sm *corpus.SourceMap
	var err error
	if *sourceMapPath != "" {
		smData, err = os.ReadFile(*sourceMapPath)
		if err != nil {
			fatalf("corpus build: %v", err)
		}
		sm, err = corpus.ParseSourceMap(smData)
	} else {
		smData, err = corpus.DefaultSourceMapBytes()
		if err != nil {
			fatalf("corpus build: %v", err)
		}
		sm, err = corpus.ParseSourceMap(smData)
	}
	if err != nil {
		fatalf("corpus build: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fatalf("corpus build: %v", err)
	}
	db, err := corpus.Open(*out)
	if err != nil {
		fatalf("corpus build: %v", err)
	}
	defer db.Close()

	result, err := corpus.Ingest(db, os.DirFS(*seclistsPath), *seclistsPath, sm, corpus.HashBytes(smData))
	if err != nil {
		fatalf("corpus build: %v", err)
	}

	fmt.Printf("corpus built at %s: %d terms from %d files", *out, result.Terms, result.Files)
	if result.SecListsCommit != "" {
		fmt.Printf(" (seclists commit %s)", result.SecListsCommit)
	}
	fmt.Println()
}

func runCorpusImport(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: smartbuster corpus import <file> --tags <a,b,...> --type dir|file [--db <path>]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("corpus import", flag.ExitOnError)
	var tags stringList
	fs.Var(&tags, "tags", "tag to attach (repeatable, or comma-separated)")
	typeStr := fs.String("type", "", "dir|file (required)")
	dbPath := fs.String("db", defaultCorpusDBPath(), "corpus DB file to import into")
	fs.Parse(args[1:])

	file := args[0]
	if file == "" || strings.HasPrefix(file, "-") || *typeStr == "" || *dbPath == "" {
		fmt.Fprintln(os.Stderr, "usage: smartbuster corpus import <file> --tags <a,b,...> --type dir|file [--db <path>]")
		os.Exit(2)
	}

	var typ corpus.TermType
	switch *typeStr {
	case "dir":
		typ = corpus.TypeDir
	case "file":
		typ = corpus.TypeFile
	default:
		fatalf("corpus import: --type must be dir or file, got %q", *typeStr)
	}

	var flatTags []string
	for _, t := range tags {
		flatTags = append(flatTags, strings.Split(t, ",")...)
	}
	if len(flatTags) == 0 {
		fatalf("corpus import: --tags is required")
	}

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		fatalf("corpus import: %v", err)
	}
	db, err := corpus.Open(*dbPath)
	if err != nil {
		fatalf("corpus import: %v", err)
	}
	defer db.Close()

	n, err := corpus.ImportUserList(db, file, flatTags, typ)
	if err != nil {
		fatalf("corpus import: %v", err)
	}
	fmt.Printf("imported %d terms from %s into %s (tags: %s)\n", n, file, *dbPath, strings.Join(flatTags, ","))
}

func runRuleset(args []string) {
	fs := flag.NewFlagSet("ruleset", flag.ExitOnError)
	if len(args) == 0 || args[0] != "update" {
		fmt.Fprintln(os.Stderr, "usage: smartbuster ruleset update --repo <url> --commit <ref> [--dest <dir>]")
		os.Exit(2)
	}
	repo := fs.String("repo", "", "git URL of the ruleset repo to pull (required)")
	commit := fs.String("commit", "", "pinned commit/ref to check out (required)")
	dest := fs.String("dest", defaultSystemRulesetDir(), "system ruleset directory to write into")
	fs.Parse(args[1:])

	if err := profile.Update(profile.UpdateOptions{Repo: *repo, Commit: *commit, Dest: *dest}); err != nil {
		fatalf("ruleset update: %v", err)
	}
	fmt.Printf("ruleset updated in %s (commit %s)\n", *dest, *commit)
}

func defaultSystemRulesetDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "smartbuster", "rules")
}

// stringList accumulates repeatable flag values (flag.Value).
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// splitFlagsAndPositional separates flag tokens (and their values) from
// positional arguments so they can appear in any order before being handed
// to flag.FlagSet, which otherwise stops parsing at the first non-flag token.
func splitFlagsAndPositional(args []string, boolFlags map[string]bool) (flagArgs, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-" || !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flagArgs = append(flagArgs, a)
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			continue // value embedded, e.g. -w=list.txt
		}
		if boolFlags[name] {
			continue // no separate value token
		}
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return flagArgs, positional
}

func runScan(args []string) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	wordlistPath := fs.String("w", "", "path to a flat wordlist file; bypasses the corpus and tags every entry \"generic\" (spec §0 contract G). \"\" = use the tagged corpus (default)")
	mode := fs.String("mode", "normal", "stealth tier preset: fast|normal|quiet|stealth (spec §2); individual flags below override the selected preset's fields")
	concurrency := fs.Int("c", 0, "number of concurrent workers; 0 = the selected --mode's own default")
	rate := fs.Float64("rate", 0, "max requests/sec across all workers; 0 = the selected --mode's own default (unbounded for fast/normal)")
	jitter := fs.Float64("jitter", 0, "fractional uniform jitter applied to the pacing interval; 0 = the selected --mode's own jitter distribution")
	jitterKind := fs.String("jitter-kind", "", "override the selected --mode's jitter distribution kind: none|uniform|gaussian|bursty")
	headerProfile := fs.String("header-profile", "", "override the selected --mode's header profile: minimal|chrome|firefox|safari")
	fingerprint := fs.String("fingerprint", "", "TLS/HTTP-2 browser fingerprint mimicry (spec §6, tier 3): chrome|firefox|safari; \"\" = the selected --mode's own default (off for fast/normal/quiet, chrome for stealth)")
	proxy := fs.String("proxy", "", "route every on-target request through this upstream proxy (http/https/socks5), e.g. http://127.0.0.1:8080 or socks5://127.0.0.1:9050 (spec §5); \"\" = direct connection")
	budget := fs.Duration("budget", 0, "spread the scan over roughly this much wall-clock time (time-budget pacing, spec §3); 0 = off")
	maxDepth := fs.Int("depth", engine.DefaultMaxDepth, "max recursion depth")
	requestTO := fs.Duration("timeout", engine.DefaultRequestTO, "per-request timeout")
	seed := fs.Int64("seed", 0, "RNG seed for reproducible runs; 0 = random, time-based")
	dryRun := fs.Bool("dry-run", false, "print the requests that would be sent, without sending them")
	outDir := fs.String("out", "smartbuster-out", "output directory for the audit log and result exports")
	savePath := fs.String("save", "", "write a resumable session snapshot (spec §6) to this path, periodically while the scan runs (see --autosave)")
	autosave := fs.Duration("autosave", 30*time.Second, "how often to write --save's snapshot; only takes effect when --save is set")

	rulesetDir := fs.String("ruleset-dir", "", "system ruleset directory (overlays the embedded defaults); \"\" = embedded only")
	userRulesDir := fs.String("user-rules-dir", "", "user ruleset overlay directory (highest precedence); \"\" = none")
	var rulesOff stringList
	fs.Var(&rulesOff, "rules-off", fmt.Sprintf("rule category to suppress (repeatable); default %v", profile.DefaultRulesOff))
	nmapFile := fs.String("nmap", "", "path to an nmap -oX XML file to ingest for target profiling")
	runNmap := fs.Bool("run-nmap", false, "opt-in: shell out to nmap -sV --script http-enum,http-headers,ssl-cert (requires nmap on PATH)")
	activeProbes := fs.Bool("active-probes", false, "fire confirmer requests (e.g. /wp-login.php) for mid-confidence tech detections")
	faviconProbe := fs.Bool("favicon-probe", true, "fetch /favicon.ico during target profiling")

	corpusDB := fs.String("corpus-db", "", "path to a prebuilt corpus DB (see `smartbuster corpus build`); \"\" = embedded minimal corpus")
	corpusMax := fs.Int("corpus-max", 0, "max candidates corpus.Select returns; 0 = unbounded")
	techBoostW := fs.Float64("tech-boost-w", corpus.DefaultTechBoostW, "TECH_BOOST_W: how strongly detected tech boosts matching candidates")

	wSem := fs.Float64("w-sem", engine.DefaultWSem, "WSem: response-semantics signal weight (spec §7)")
	wAssoc := fs.Float64("w-assoc", engine.DefaultWAssoc, "WAssoc: association/companion signal weight (spec §7)")
	wConv := fs.Float64("w-conv", engine.DefaultWConv, "WConv: naming-convention (Markov) signal weight (spec §7)")
	markovOrder := fs.Int("markov-order", engine.DefaultMarkovOrder, "MARKOV_ORDER: naming-convention model's char n-gram order")
	markovMinSamples := fs.Int("markov-min-samples", engine.DefaultMarkovMinSamples, "MARKOV_MIN_SAMPLES: cold-start threshold before the naming-convention signal activates")
	learnMinConf := fs.Float64("learn-min-conf", engine.DefaultLearnMinConf, "LEARN_MIN_CONF: min confidence for a hit to feed the dynamic learners (poisoning defense)")
	subtreeBurst := fs.Int("subtree-burst", engine.DefaultSubtreeBurst, "SUBTREE_BURST: consecutive requests a directory may run before yielding to another")
	epsilon := fs.Float64("epsilon", 0, "EPSILON: ε-greedy exploration probability; 0 = pure greedy (every mode's default — exploration ties randomness to live dispatch timing, breaking seed reproducibility, so it's opt-in only)")
	reprioHits := fs.Int("reprio-hits", engine.DefaultReprioHits, "REPRIO_INTERVAL: reprioritize the frontier after this many qualifying hits")
	reprioInterval := fs.Duration("reprio-interval", engine.DefaultReprioInterval, "REPRIO_INTERVAL: or after this much elapsed time, whichever first")

	robots := fs.Bool("robots", true, "seed from robots.txt Disallow/Allow directives (spec §5.1)")
	sitemap := fs.Bool("sitemap", true, "seed from sitemap.xml, incl. sitemaps declared in robots.txt (spec §5.2)")
	wayback := fs.Bool("wayback", false, "seed from the Wayback Machine/CDX; opt-in, off-target network call (spec §5.3)")
	waybackMax := fs.Int("wayback-max", passiveseed.WaybackMaxDefault, "WAYBACK_MAX: CDX row cap before scope/asset/dedup filtering")
	seedAssets := fs.Bool("seed-assets", false, "keep static-asset noise (.png/.css/...) from Wayback seeds")

	crawl := fs.Bool("crawl", true, "harvest HTML links from responses the scan already made (spec §3); near-free, no extra page fetches")
	jsHarvest := fs.Bool("js-harvest", true, "fetch and mine JS bundles for endpoints (spec §4); bounded, shares the worker pool")
	headless := fs.Bool("headless", false, "opt-in: capture live XHR/fetch URLs and rendered routes via a headless browser (spec §6); requires playwright installed, degrades gracefully if absent")
	crawlDepth := fs.Int("crawl-depth", 0, "CrawlDepth: max path depth for crawl/JS/headless seeds; 0 = same as -depth")

	var allowHosts, excludeHosts, excludePatterns stringList
	fs.Var(&allowHosts, "allow-host", "additional in-scope host (repeatable); defaults to the target(s)' own host")
	fs.Var(&excludeHosts, "exclude-host", "host to exclude from scope (repeatable)")
	fs.Var(&excludePatterns, "exclude-pattern", "regex on the URL path to exclude (repeatable)")

	// The spec's own invocation ("smartbuster scan <host> -w list.txt") puts
	// the positional target before the flags, but Go's flag package stops
	// parsing at the first non-flag token. Reorder so flags and positionals
	// can appear in any order, matching normal CLI conventions.
	flagArgs, targets := splitFlagsAndPositional(args, map[string]bool{
		"dry-run": true, "run-nmap": true, "active-probes": true, "favicon-probe": true,
		"robots": true, "sitemap": true, "wayback": true, "seed-assets": true,
		"crawl": true, "js-harvest": true, "headless": true,
	})
	fs.Parse(flagArgs)
	if len(targets) == 0 {
		usage()
		fs.PrintDefaults()
		os.Exit(2)
	}
	if *seed == 0 {
		*seed = time.Now().UnixNano()
	}

	// -w bypasses the corpus (spec §0 contract G); with no -w, entries stays
	// empty and the coordinator seeds from the corpus instead.
	var entries []wordlist.Entry
	var wlHash string
	if *wordlistPath != "" {
		var err error
		entries, err = wordlist.Load(*wordlistPath)
		if err != nil {
			fatalf("wordlist: %v", err)
		}
		wlHash, err = wordlist.Hash(*wordlistPath)
		if err != nil {
			fatalf("wordlist: %v", err)
		}
	}

	sc := buildScope(targets, allowHosts, excludeHosts, excludePatterns)

	cfg := engine.Config{
		Targets: targets, Wordlist: *wordlistPath, Concurrency: *concurrency,
		Mode: *mode, Budget: *budget, JitterKind: *jitterKind, HeaderProfile: *headerProfile,
		Fingerprint: *fingerprint, Proxy: *proxy,
		Rate: *rate, Jitter: *jitter, MaxDepth: *maxDepth, RequestTO: *requestTO,
		Seed: *seed, DryRun: *dryRun, OutDir: *outDir,
		RulesetDir: *rulesetDir, UserRulesDir: *userRulesDir, RulesOff: rulesOff,
		NmapFile: *nmapFile, RunNmap: *runNmap, ActiveProbes: *activeProbes, FaviconProbe: *faviconProbe,
		CorpusDB: *corpusDB, CorpusMax: *corpusMax, TechBoostW: *techBoostW,
		Weights:          engine.ScoreWeights{WSem: *wSem, WAssoc: *wAssoc, WConv: *wConv},
		MarkovOrder:      *markovOrder,
		MarkovMinSamples: *markovMinSamples,
		LearnMinConf:     *learnMinConf,
		SubtreeBurst:     *subtreeBurst,
		Epsilon:          *epsilon,
		ReprioHits:       *reprioHits,
		ReprioInterval:   *reprioInterval,
		Robots:           *robots,
		Sitemap:          *sitemap,
		Wayback:          *wayback,
		WaybackMax:       *waybackMax,
		SeedAssets:       *seedAssets,
		Crawl:            *crawl,
		JSHarvest:        *jsHarvest,
		Headless:         *headless,
		CrawlDepth:       *crawlDepth,
		SavePath:         *savePath,
		Autosave:         *autosave,
	}

	if *dryRun {
		runDryRun(targets, entries, cfg, sc, *seed)
		return
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatalf("output dir: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// nmap can reveal additional web ports on a host already being scanned
	// (spec §7 multi-port services); when it's in play, always use
	// per-target subdirectories, since the queue below may grow past the
	// targets the user actually typed.
	multiOutputs := len(targets) > 1 || *nmapFile != "" || *runNmap

	var allFindings []engine.Finding
	visited := make(map[string]bool)
	queue := append([]string(nil), targets...)
	for i := 0; i < len(queue); i++ {
		target := queue[i]
		if visited[target] {
			continue
		}
		visited[target] = true

		dir := *outDir
		if multiOutputs {
			dir = filepath.Join(*outDir, sanitizeHost(target, i))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fatalf("output dir: %v", err)
			}
		}
		findings, discovered := scanOne(ctx, target, entries, cfg, sc, dir, wlHash)
		allFindings = append(allFindings, findings...)
		for _, svc := range discovered {
			if !visited[svc] {
				queue = append(queue, svc)
			}
		}
	}

	fmt.Println()
	output.WriteTree(os.Stdout, allFindings)
}

// buildScope defaults the allowlist to the targets' own hosts, so recursion
// can never wander off-host by default, while letting -allow-host widen or
// -exclude-host/-exclude-pattern narrow that.
func buildScope(targets []string, allowHosts, excludeHosts, excludePatterns stringList) *scope.Scope {
	scopeAllow := append([]string(nil), allowHosts...)
	if len(scopeAllow) == 0 {
		for _, t := range targets {
			h, err := hostOf(t)
			if err != nil {
				fatalf("target %q: %v", t, err)
			}
			scopeAllow = append(scopeAllow, h)
		}
	}
	sc, err := scope.New(scope.Config{
		AllowHosts: scopeAllow, ExcludeHosts: excludeHosts, ExcludePatterns: excludePatterns,
	})
	if err != nil {
		fatalf("scope: %v", err)
	}
	return sc
}

func runDryRun(targets []string, entries []wordlist.Entry, cfg engine.Config, sc *scope.Scope, seed int64) {
	for _, t := range targets {
		t = strings.TrimRight(t, "/")
		if !sc.InScope(t) {
			fmt.Printf("[dry-run] refused (out of scope): %s\n", t)
			continue
		}
		urls := engine.PreviewRequests(t, entries, seed)
		if len(entries) == 0 {
			var err error
			urls, err = engine.PreviewRequestsCorpus(t, cfg, seed)
			if err != nil {
				fatalf("dry-run: %v", err)
			}
		}
		for _, u := range urls {
			fmt.Printf("[dry-run] GET %s\n", u)
		}
	}
}

// runServe implements `smartbuster serve` (spec §2, §7): binds the
// loopback daemon, prints its URL with the session token, optionally opens
// a browser, and serves REST + WS + the (5b) static asset mount until
// interrupted.
func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 0, "port to listen on; 0 = OS-assigned")
	bind := fs.String("bind", "127.0.0.1", "address to bind (spec §5: refused unless loopback, without --i-know-this-is-remote)")
	open := fs.Bool("open", false, "launch a browser at the daemon's URL once it's listening")
	iKnowThisIsRemote := fs.Bool("i-know-this-is-remote", false, "allow a non-loopback --bind (dangerous: this daemon can initiate scans against arbitrary hosts)")
	sessionDir := fs.String("session-dir", "", "directory session files are saved to/listed from; \"\" = a per-user config directory")
	fs.Parse(args)

	d, err := daemon.Start(daemon.Options{
		Bind: *bind, Port: *port, SessionDir: *sessionDir, AllowRemote: *iKnowThisIsRemote,
	})
	if err != nil {
		fatalf("serve: %v", err)
	}

	fmt.Printf("smartbuster daemon listening on %s\n", d.URL)
	fmt.Printf("open: %s\n", d.TokenURL)

	if *open {
		if err := openBrowser(d.TokenURL); err != nil {
			fmt.Fprintf(os.Stderr, "smartbuster: --open: %v (open the URL above manually)\n", err)
		}
	}

	if err := d.Serve(); err != nil {
		fatalf("serve: %v", err)
	}
}

// openBrowser shells out to the platform's "open this URL" command. Best
// effort: serve already printed the URL, so a failure here just means the
// user opens it by hand.
func openBrowser(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{url}
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		cmd, args = "xdg-open", []string{url}
	}
	return exec.Command(cmd, args...).Start()
}

// runResume implements `smartbuster resume <session-file.json>` (spec §6):
// rebuild the coordinator from a saved SessionState and continue scanning
// — the CLI counterpart to POST /api/sessions/{id}/resume.
func runResume(args []string) {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	outDir := fs.String("out", "smartbuster-out", "output directory for the audit log and result exports")
	savePath := fs.String("save", "", "write a resumable session snapshot (spec §6) to this path, periodically while the scan runs")
	autosave := fs.Duration("autosave", 30*time.Second, "how often to write --save's snapshot; only takes effect when --save is set")
	fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: smartbuster resume <session-file.json> [flags]")
		os.Exit(2)
	}
	sessionFile := fs.Arg(0)

	data, err := os.ReadFile(sessionFile)
	if err != nil {
		fatalf("resume: %v", err)
	}
	var state engine.SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		fatalf("resume: %v", err)
	}
	state.Config.SavePath = *savePath
	state.Config.Autosave = *autosave

	var entries []wordlist.Entry
	if state.Config.Wordlist != "" {
		entries, err = wordlist.Load(state.Config.Wordlist)
		if err != nil {
			fatalf("resume: wordlist: %v", err)
		}
	}
	sc := buildScope([]string{state.Target}, nil, nil, nil)

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatalf("output dir: %v", err)
	}
	auditWriter, err := audit.New(filepath.Join(*outDir, "audit.jsonl"))
	if err != nil {
		fatalf("audit log: %v", err)
	}
	defer auditWriter.Close()
	if err := auditWriter.WriteHeader(audit.Header{
		Version: version, Targets: []string{state.Target}, Wordlist: state.Config.Wordlist,
		UserAgent: httpclient.DefaultUserAgent, Seed: state.Config.Seed,
		Concurrency: state.Config.Concurrency, Rate: state.Config.Rate, Jitter: state.Config.Jitter,
		MaxDepth: state.Config.MaxDepth, RequestTOMs: state.Config.RequestTO.Milliseconds(),
	}); err != nil {
		fatalf("audit log: %v", err)
	}

	co, err := engine.NewCoordinatorFromSnapshot(state, entries, sc,
		engine.WithAuditSink(auditWriter), engine.WithEventEmitter(cliEmitter()))
	if err != nil {
		fatalf("resume: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Printf("resuming %s from %s (seed=%d, %d findings so far)\n", state.Target, sessionFile, state.Config.Seed, len(state.Findings))
	runWithAutosave(ctx, co, state.Config)

	findings := co.Findings()
	if err := writeResultFile(filepath.Join(*outDir, "results.json"), func(w io.Writer) error {
		return output.WriteJSON(w, findings)
	}); err != nil {
		fatalf("results.json: %v", err)
	}
	if err := writeResultFile(filepath.Join(*outDir, "results.txt"), func(w io.Writer) error {
		return output.WritePlaintext(w, findings)
	}); err != nil {
		fatalf("results.txt: %v", err)
	}
	fmt.Println()
	output.WriteTree(os.Stdout, findings)
}

// cliEmitter is the console event printer shared by every CLI entry point
// (scan, resume) — 5b's daemon has its own sink (the WS hub), so this is
// specifically the terminal-facing one.
func cliEmitter() engine.EventEmitter {
	return engine.EventFunc(func(e engine.Event) {
		switch e.Type {
		case engine.EventHit:
			fmt.Printf("[hit] %s (conf: %.2f) %s\n", e.URL, e.Confidence, e.Message)
		case engine.EventWarning:
			fmt.Printf("[warn] %s: %s\n", e.Dir, e.Message)
		case engine.EventThrottle:
			fmt.Printf("[throttle] %s\n", e.Message)
		case engine.EventTrapDetected:
			fmt.Printf("[trap] %s: %s\n", e.Dir, e.Message)
		case engine.EventBranchPruned:
			fmt.Printf("[pruned] %s: %s\n", e.Dir, e.Message)
		case engine.EventError:
			fmt.Printf("[error] %s: %s\n", e.URL, e.Message)
		case engine.EventTechDetected:
			for _, tech := range e.Tech {
				fmt.Printf("[tech] %s (%s, conf: %.2f, layer: %s)\n", tech.Name, tech.Category, tech.Confidence, tech.Layer)
			}
		case engine.EventWAFDetected:
			fmt.Printf("[waf] %s\n", e.WAF)
		case engine.EventSPAPivot:
			fmt.Printf("[spa.pivot] %s: brute-force deprioritized, harvesting root for the real API surface\n", e.URL)
		}
	})
}

// runWithAutosave runs co to completion, writing a session snapshot to
// cfg.SavePath every cfg.Autosave interval while it runs (spec §7's CLI
// --save/--autosave): the ticker goroutine stops the moment Run returns,
// since co.Save (spec §6) only works while the coordinator's dispatchLoop
// is still alive to service it.
func runWithAutosave(ctx context.Context, co *engine.Coordinator, cfg engine.Config) {
	if cfg.SavePath != "" && cfg.Autosave > 0 {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			ticker := time.NewTicker(cfg.Autosave)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					saveSession(co, cfg.SavePath)
				}
			}
		}()
	}
	co.Run(ctx)
}

// saveSession writes one session snapshot to path (spec §6's on-disk
// format: inspectable JSON, matching the audit ethos). Failures are
// reported but non-fatal — a failed autosave shouldn't abort a scan that's
// otherwise proceeding fine.
func saveSession(co *engine.Coordinator, path string) {
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, err := co.Save(saveCtx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smartbuster: session save: %v\n", err)
		return
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smartbuster: session save: %v\n", err)
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(state); err != nil {
		fmt.Fprintf(os.Stderr, "smartbuster: session save: %v\n", err)
	}
}

// scanOne runs one scan and returns its findings plus any additional
// same-host web service base URLs nmap revealed (spec §7); the caller
// decides whether to queue those for their own scan.
func scanOne(ctx context.Context, target string, entries []wordlist.Entry, cfg engine.Config, sc *scope.Scope, outDir, wlHash string) ([]engine.Finding, []string) {
	auditWriter, err := audit.New(filepath.Join(outDir, "audit.jsonl"))
	if err != nil {
		fatalf("audit log: %v", err)
	}
	defer auditWriter.Close()

	if err := auditWriter.WriteHeader(audit.Header{
		Version: version, Targets: []string{target}, Wordlist: cfg.Wordlist,
		WordlistHash: wlHash, UserAgent: httpclient.DefaultUserAgent, Seed: cfg.Seed,
		Concurrency: cfg.Concurrency, Rate: cfg.Rate, Jitter: cfg.Jitter,
		MaxDepth: cfg.MaxDepth, RequestTOMs: cfg.RequestTO.Milliseconds(), DryRun: cfg.DryRun,
	}); err != nil {
		fatalf("audit log: %v", err)
	}

	co, err := engine.NewCoordinator(target, entries, cfg, sc,
		engine.WithAuditSink(auditWriter), engine.WithEventEmitter(cliEmitter()))
	if err != nil {
		fatalf("%v", err)
	}

	fmt.Printf("scanning %s (seed=%d)\n", target, cfg.Seed)
	runWithAutosave(ctx, co, cfg)

	findings := co.Findings()
	if err := writeResultFile(filepath.Join(outDir, "results.json"), func(w io.Writer) error {
		return output.WriteJSON(w, findings)
	}); err != nil {
		fatalf("results.json: %v", err)
	}
	if err := writeResultFile(filepath.Join(outDir, "results.txt"), func(w io.Writer) error {
		return output.WritePlaintext(w, findings)
	}); err != nil {
		fatalf("results.txt: %v", err)
	}
	return findings, co.DiscoveredServices()
}

func writeResultFile(path string, write func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return write(f)
}

func hostOf(target string) (string, error) {
	u, err := url.Parse(target)
	if err != nil {
		return "", err
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("no host in %q", target)
	}
	return u.Hostname(), nil
}

func sanitizeHost(target string, idx int) string {
	h, err := hostOf(target)
	if err != nil || h == "" {
		return fmt.Sprintf("target-%d", idx)
	}
	return fmt.Sprintf("%s-%d", strings.NewReplacer(":", "_", "/", "_").Replace(h), idx)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "smartbuster: "+format+"\n", args...)
	os.Exit(1)
}

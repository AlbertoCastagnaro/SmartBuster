// Command smartbuster is the CLI entry point: flag parsing and wiring for
// the engine, audit log, and result output.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/audit"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/output"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
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
	case "ruleset":
		runRuleset(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "smartbuster: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: smartbuster scan <target> [<target>...] -w <wordlist> [flags]")
	fmt.Fprintln(os.Stderr, "       smartbuster ruleset update --repo <url> --commit <ref> [--dest <dir>]")
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
	wordlistPath := fs.String("w", "", "path to the wordlist file (required)")
	concurrency := fs.Int("c", engine.DefaultConcurrency, "number of concurrent workers")
	rate := fs.Float64("rate", 0, "max requests/sec across all workers; 0 = unbounded")
	jitter := fs.Float64("jitter", httpclient.DefaultJitter, "fractional jitter applied to the pacing interval")
	maxDepth := fs.Int("depth", engine.DefaultMaxDepth, "max recursion depth")
	requestTO := fs.Duration("timeout", engine.DefaultRequestTO, "per-request timeout")
	seed := fs.Int64("seed", 0, "RNG seed for reproducible runs; 0 = random, time-based")
	dryRun := fs.Bool("dry-run", false, "print the requests that would be sent, without sending them")
	outDir := fs.String("out", "smartbuster-out", "output directory for the audit log and result exports")

	rulesetDir := fs.String("ruleset-dir", "", "system ruleset directory (overlays the embedded defaults); \"\" = embedded only")
	userRulesDir := fs.String("user-rules-dir", "", "user ruleset overlay directory (highest precedence); \"\" = none")
	var rulesOff stringList
	fs.Var(&rulesOff, "rules-off", fmt.Sprintf("rule category to suppress (repeatable); default %v", profile.DefaultRulesOff))
	nmapFile := fs.String("nmap", "", "path to an nmap -oX XML file to ingest for target profiling")
	runNmap := fs.Bool("run-nmap", false, "opt-in: shell out to nmap -sV --script http-enum,http-headers,ssl-cert (requires nmap on PATH)")
	activeProbes := fs.Bool("active-probes", false, "fire confirmer requests (e.g. /wp-login.php) for mid-confidence tech detections")
	faviconProbe := fs.Bool("favicon-probe", true, "fetch /favicon.ico during target profiling")

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
	})
	fs.Parse(flagArgs)
	if *wordlistPath == "" || len(targets) == 0 {
		usage()
		fs.PrintDefaults()
		os.Exit(2)
	}
	if *seed == 0 {
		*seed = time.Now().UnixNano()
	}

	entries, err := wordlist.Load(*wordlistPath)
	if err != nil {
		fatalf("wordlist: %v", err)
	}
	wlHash, err := wordlist.Hash(*wordlistPath)
	if err != nil {
		fatalf("wordlist: %v", err)
	}

	sc := buildScope(targets, allowHosts, excludeHosts, excludePatterns)

	cfg := engine.Config{
		Targets: targets, Wordlist: *wordlistPath, Concurrency: *concurrency,
		Rate: *rate, Jitter: *jitter, MaxDepth: *maxDepth, RequestTO: *requestTO,
		Seed: *seed, DryRun: *dryRun, OutDir: *outDir,
		RulesetDir: *rulesetDir, UserRulesDir: *userRulesDir, RulesOff: rulesOff,
		NmapFile: *nmapFile, RunNmap: *runNmap, ActiveProbes: *activeProbes, FaviconProbe: *faviconProbe,
	}

	if *dryRun {
		runDryRun(targets, entries, sc, *seed)
		return
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatalf("output dir: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var allFindings []engine.Finding
	for i, target := range targets {
		dir := *outDir
		if len(targets) > 1 {
			dir = filepath.Join(*outDir, sanitizeHost(target, i))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fatalf("output dir: %v", err)
			}
		}
		allFindings = append(allFindings, scanOne(ctx, target, entries, cfg, sc, dir, wlHash)...)
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

func runDryRun(targets []string, entries []wordlist.Entry, sc *scope.Scope, seed int64) {
	for _, t := range targets {
		t = strings.TrimRight(t, "/")
		if !sc.InScope(t) {
			fmt.Printf("[dry-run] refused (out of scope): %s\n", t)
			continue
		}
		for _, u := range engine.PreviewRequests(t, entries, seed) {
			fmt.Printf("[dry-run] GET %s\n", u)
		}
	}
}

func scanOne(ctx context.Context, target string, entries []wordlist.Entry, cfg engine.Config, sc *scope.Scope, outDir, wlHash string) []engine.Finding {
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

	emitter := engine.EventFunc(func(e engine.Event) {
		switch e.Type {
		case engine.EventHit:
			fmt.Printf("[hit] %s (conf: %.2f) %s\n", e.URL, e.Confidence, e.Message)
		case engine.EventWarning:
			fmt.Printf("[warn] %s: %s\n", e.Dir, e.Message)
		case engine.EventThrottle:
			fmt.Printf("[throttle] %s\n", e.Message)
		case engine.EventTrapDetected:
			fmt.Printf("[trap] %s: %s\n", e.Dir, e.Message)
		case engine.EventTechDetected:
			for _, tech := range e.Tech {
				fmt.Printf("[tech] %s (%s, conf: %.2f, layer: %s)\n", tech.Name, tech.Category, tech.Confidence, tech.Layer)
			}
		case engine.EventWAFDetected:
			fmt.Printf("[waf] %s\n", e.WAF)
		}
	})

	co, err := engine.NewCoordinator(target, entries, cfg, sc,
		engine.WithAuditSink(auditWriter), engine.WithEventEmitter(emitter))
	if err != nil {
		fatalf("%v", err)
	}

	fmt.Printf("scanning %s (seed=%d)\n", target, cfg.Seed)
	co.Run(ctx)

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
	return findings
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

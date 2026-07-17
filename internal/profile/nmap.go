package profile

import (
	"context"
	"encoding/xml"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

const (
	// NmapProbeConf / NmapTableConf are the confidences a -sV version match
	// scales to depending on nmap's own reported method (spec §7, §8).
	NmapProbeConf = 0.9
	NmapTableConf = 0.4
	// nmapPortHeuristicConf is used only when there's no -sV data to go on.
	nmapPortHeuristicConf = 0.35
)

var webServiceNames = map[string]bool{
	"http": true, "https": true, "http-proxy": true, "http-alt": true, "https-alt": true,
}

// portHeuristics maps a well-known dev-server port to the tech it usually
// means, used only when nmap has no -sV product/version for that port
// (spec §7).
var portHeuristics = map[int]struct{ name, category string }{
	8080: {"Apache Tomcat", "server"},
	8443: {"HTTPS (alt)", "server"},
	3000: {"Node.js", "language"},
	5000: {"Flask", "framework"},
	8000: {"Django", "framework"},
	9200: {"Elasticsearch", "server"},
	9000: {"PHP-FPM", "language"},
}

type nmapRun struct {
	XMLName xml.Name   `xml:"nmaprun"`
	Hosts   []nmapHost `xml:"host"`
}

type nmapHost struct {
	Addresses []nmapAddress `xml:"address"`
	Hostnames struct {
		Hostname []nmapHostname `xml:"hostname"`
	} `xml:"hostnames"`
	Ports struct {
		Port []nmapPort `xml:"port"`
	} `xml:"ports"`
}

type nmapAddress struct {
	Addr     string `xml:"addr,attr"`
	AddrType string `xml:"addrtype,attr"`
}

type nmapHostname struct {
	Name string `xml:"name,attr"`
}

type nmapPort struct {
	Protocol string `xml:"protocol,attr"`
	PortID   int    `xml:"portid,attr"`
	State    struct {
		State string `xml:"state,attr"`
	} `xml:"state"`
	Service nmapService  `xml:"service"`
	Scripts []nmapScript `xml:"script"`
}

type nmapService struct {
	Name    string `xml:"name,attr"`
	Product string `xml:"product,attr"`
	Version string `xml:"version,attr"`
	Method  string `xml:"method,attr"`
	Conf    string `xml:"conf,attr"`
	Tunnel  string `xml:"tunnel,attr"`
}

type nmapScript struct {
	ID     string `xml:"id,attr"`
	Output string `xml:"output,attr"`
}

// NmapTechVote is a tech signal derived from an nmap scan, to be applied to
// a TargetProfile via TargetProfile.vote (kept separate so package nmap
// parsing has no dependency the caller doesn't already have).
type NmapTechVote struct {
	Name       string
	Category   string
	Confidence float64
	Layer      Layer
}

// NmapSeed is a frontier candidate seed derived from NSE output (spec §7):
// http-enum and http-robots.txt paths. Confirmed/rejected by calibration
// like any other candidate — never reported as a finding directly.
type NmapSeed struct {
	Path       string // relative to the service's root, no leading slash
	Provenance string
}

// VoteNmap applies an nmap-derived tech vote (spec §7: "nmap tech is a
// prior, confirmed/overridden by live signals").
func (p *TargetProfile) VoteNmap(v NmapTechVote) {
	p.vote(v.Name, v.Category, v.Layer, SrcNmap, v.Confidence, "", "nmap")
}

// NmapResult is one host's ingested nmap data, already filtered to
// in-scope services (spec §7: "every derived target passes the scope
// enforcer; out-of-scope hosts are dropped with a warning").
type NmapResult struct {
	Host     string
	Services []ServiceTarget
	Votes    []NmapTechVote
	Seeds    []NmapSeed
	VHosts   []string
}

// IngestNmap parses nmap -oX output and returns one NmapResult per host
// that inScope allows; out-of-scope hosts are skipped and reported in
// warnings (spec §7).
func IngestNmap(data []byte, inScope func(host string) bool) (results []NmapResult, warnings []string, err error) {
	var run nmapRun
	if err := xml.Unmarshal(data, &run); err != nil {
		return nil, nil, fmt.Errorf("parse nmap xml: %w", err)
	}

	for _, h := range run.Hosts {
		host := primaryAddress(h)
		if host == "" {
			continue
		}
		if inScope != nil && !inScope(host) {
			warnings = append(warnings, fmt.Sprintf("nmap: host %s dropped (out of scope)", host))
			continue
		}
		results = append(results, ingestHost(host, h))
	}
	return results, warnings, nil
}

func primaryAddress(h nmapHost) string {
	for _, a := range h.Addresses {
		if a.AddrType == "ipv4" || a.AddrType == "ipv6" {
			return a.Addr
		}
	}
	if len(h.Addresses) > 0 {
		return h.Addresses[0].Addr
	}
	if len(h.Hostnames.Hostname) > 0 {
		return h.Hostnames.Hostname[0].Name
	}
	return ""
}

func ingestHost(host string, h nmapHost) NmapResult {
	res := NmapResult{Host: host}

	for _, port := range h.Ports.Port {
		if port.State.State != "" && port.State.State != "open" {
			continue
		}
		if !webServiceNames[port.Service.Name] {
			continue
		}

		scheme := "http"
		if strings.Contains(port.Service.Name, "https") || port.Service.Tunnel == "ssl" || port.PortID == 443 || port.PortID == 8443 {
			scheme = "https"
		}
		res.Services = append(res.Services, ServiceTarget{
			BaseURL: fmt.Sprintf("%s://%s:%d", scheme, host, port.PortID),
			Port:    port.PortID,
			Scheme:  scheme,
		})

		if port.Service.Product != "" {
			conf := NmapTableConf
			if port.Service.Method == "probe" {
				if n, err := strconv.Atoi(port.Service.Conf); err == nil && n >= 8 {
					conf = NmapProbeConf
				}
			}
			res.Votes = append(res.Votes, NmapTechVote{Name: port.Service.Product, Category: "server", Confidence: conf, Layer: LayerBackend})
		} else if h, ok := portHeuristics[port.PortID]; ok {
			res.Votes = append(res.Votes, NmapTechVote{Name: h.name, Category: h.category, Confidence: nmapPortHeuristicConf, Layer: LayerUnknown})
		}

		for _, script := range port.Scripts {
			switch script.ID {
			case "http-enum":
				for _, p := range extractPaths(script.Output) {
					res.Seeds = append(res.Seeds, NmapSeed{Path: p, Provenance: "nmap:http-enum"})
				}
			case "http-robots-txt", "http-robots.txt":
				for _, p := range extractPaths(script.Output) {
					res.Seeds = append(res.Seeds, NmapSeed{Path: p, Provenance: "nmap:http-robots.txt"})
				}
			case "http-server-header":
				if name := firstLine(script.Output); name != "" {
					res.Votes = append(res.Votes, NmapTechVote{Name: name, Category: "server", Confidence: NmapTableConf, Layer: LayerEdge})
				}
			case "ssl-cert":
				res.VHosts = append(res.VHosts, extractSANs(script.Output)...)
			}
		}
	}
	return res
}

var pathToken = regexp.MustCompile(`/[^\s,:]+`)

func extractPaths(output string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		m := pathToken.FindString(line)
		if m == "" {
			continue
		}
		p := strings.TrimPrefix(m, "/")
		p = strings.TrimSuffix(p, "/")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func firstLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

var sanToken = regexp.MustCompile(`DNS:([^,\s]+)`)

func extractSANs(output string) []string {
	matches := sanToken.FindAllStringSubmatch(output, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// RunNmap orchestrates a live nmap scan (spec §7's opt-in --run-nmap):
// nmap -sV --script http-enum,http-headers,ssl-cert -oX -. Requires nmap on
// PATH. -sV service/version detection does not itself require elevated
// privileges (unlike -O OS detection, which this does not use), but the
// operator's nmap installation/network policy may still require it.
func RunNmap(ctx context.Context, target string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "nmap", "-sV", "--script", "http-enum,http-headers,ssl-cert", "-oX", "-", target)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("nmap: %w: %s", err, ee.Stderr)
		}
		return nil, fmt.Errorf("nmap: %w (is nmap installed and on PATH?)", err)
	}
	return out, nil
}

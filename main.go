package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	godebug "runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"openflux/socks5"
	"openflux/transport"
	"openflux/transport/cupsonline"
	"openflux/transport/jitsi"
	"openflux/transport/mailru"
	"openflux/transport/oneme"
	"openflux/transport/yandex"
	"openflux/tunnel"
	"openflux/utils"
)

// docURLList collects --url, which may be repeated and may carry several
// documents separated by commas or whitespace. Each one becomes a link of the
// bond, so a document being closed by the provider stops mattering.
type docURLList []string

func (d *docURLList) String() string { return strings.Join(*d, ",") }

func (d *docURLList) Set(v string) error {
	*d = append(*d, parseDocumentList(v)...)
	return nil
}

var docURLs docURLList

// parseDocumentList reads document URLs out of free-form text: one per line,
// several per line separated by commas or spaces, blank lines skipped, and
// everything after a '#' treated as a comment. This is what lets a node keep
// its documents in a file instead of in a command line with ten flags on it.
func parseDocumentList(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		for _, part := range strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || r == '\r' || r == '\t' || r == ' '
		}) {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

var (
	globalDocUrl string
	maxToken     string
	maxUid       string
	localIP      string
)

// expandShortFlags rewrites single-letter flag aliases into their long
// forms so both -r and --role work. Handles bare flags (-d) and inline
// values (-r=exit, -u=https://...).
func expandShortFlags(args []string) []string {
	aliases := map[string]string{
		"-r": "--role",
		"-i": "--inbound",
		"-t": "--transport",
		"-m": "--mode",
		"-c": "--codec",
		"-u": "--url",
		"-s": "--socks5",
		"-l": "--local-ip",
		"-d": "--debug",
	}
	out := make([]string, 0, len(args))
	for _, a := range args {
		replaced := false
		for short, long := range aliases {
			if a == short {
				out = append(out, long)
				replaced = true
				break
			}
			if strings.HasPrefix(a, short+"=") {
				out = append(out, long+a[len(short):])
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, a)
		}
	}
	return out
}

const (
	roleClient    = "client"
	roleExit      = "exit"
	roleBenchSend = "bench-send"
	roleBenchSink = "bench-sink"
)

const (
	inboundTUN    = "tun"
	inboundSOCKS5 = "socks5"
)

const (
	codecBatched = "batched"
	codecLegacy  = "legacy"
)

func main() {
	fmt.Print("written by p1neappleXpress\n")

	role := flag.String("role", roleClient, "client | exit | bench-send | bench-sink")
	inbound := flag.String("inbound", "", "tun | socks5 (client only; default: tun on macOS, socks5 elsewhere)")
	transportType := flag.String("transport", "yandex", "Transport type (yandex, vyandex, oneme, cupsonline, mailru, jitsi)")
	mode := flag.String("mode", "", "Exit-node mode: l3 (default, Linux only) or l4 (works everywhere)")

	codec := flag.String("codec", codecBatched, "batched (default, zstd+coalescing) or legacy (per-packet LZ4)")
	encryptionKeyFile := flag.String("encryption-key-file", "",
		"Optional: encrypt the transport with AES-256-GCM using a shared secret read from this file. "+
			"Both peers must use the same secret; unset means unencrypted, unchanged behavior")

	urlFile := flag.String("url-file", "",
		"Read document URLs from this file, one per line; '#' starts a comment. "+
			"Equivalent to repeating --url, and far easier to manage for a node "+
			"running many documents. Combines with --url if both are given")
	flag.Var(&docURLs, "url",
		"Document URL. May be repeated, or given several times over as a comma-separated "+
			"list: every document becomes a link of one bonded channel, so the tunnel "+
			"survives any single document being closed. Both peers must be given the same set")
	flag.StringVar(&maxToken, "maxToken", "", "MAX Web token. If u use MAX transport")
	flag.StringVar(&maxUid, "maxUid", "", "MAX call user id. If u use MAX transport")
	socksAddr := flag.String("socks5", ":1080", "SOCKS5 address")
	dnsServer := flag.String("dns", "",
		"Resolver to use while the tun client is up (macOS, --inbound=tun). Its UDP "+
			"queries are re-issued as DNS-over-TCP through the tunnel, so a resolver "+
			"living behind the exit node works. The previous setting is restored on exit")
	flag.StringVar(&localIP, "local-ip", "", "Egress IP for exit node (l3 mode only, scoped RST drop)")

	benchBytes := flag.Int("bench-bytes", 0, "Benchmark: push this many MB through the transport, then report and exit")
	benchCompressible := flag.Bool("bench-compressible", false, "Benchmark: use compressible payload instead of random")

	debug := flag.Bool("debug", false, "Enable verbose debug logging")

	// Deprecated aliases, kept for one release to ease migration.
	depClient := flag.Bool("client", false, "DEPRECATED: use --role=client")
	depExit := flag.Bool("exit-node", false, "DEPRECATED: use --role=exit")
	depTun := flag.Bool("tun", false, "DEPRECATED: use --inbound=tun")
	depSocks5Mode := flag.Bool("socks5-mode", false, "DEPRECATED: use --inbound=socks5")
	depLegacy := flag.Bool("legacy", false, "DEPRECATED: use --codec=legacy")
	depBenchSend := flag.Int("bench-send", 0, "DEPRECATED: use --role=bench-send --bench-bytes=N")
	depBenchSink := flag.Bool("bench-sink", false, "DEPRECATED: use --role=bench-sink")

	// Override the default flag.PrintDefaults so -h prints a structured
	// usage message with axes, modifiers, and examples instead of a flat
	// alphabetical list.
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `OpenFlux — Network stack research tool. TCP tunnel with pluggable transports.

USAGE
  openflux --role=<role> --transport=<type> [OPTIONS]

ROLE
  -r, --role=client       Run as client. (default)
  -r, --role=exit         Run as exit node.
  -r, --role=bench-send   Benchmark: push --bench-bytes MB.
  -r, --role=bench-sink   Benchmark: receive from transport.

TRANSPORT
  -t, --transport=yandex       Yandex.Docs over WebSocket. (default)
  -t, --transport=vyandex      Yandex.Volga over HTTP relay + WS.
  -t, --transport=oneme        MAX (VK) over WebRTC.
  -t, --transport=cupsonline   Cups.online interview rooms.
  -t, --transport=mailru       Mail.ru Docs over WebSocket.
  -t, --transport=jitsi        Jitsi Meet conference over colibri-ws.

  -u, --url=<URL>              Document URL. Repeat it, or pass a comma-separated
                               list, to bond several documents into one channel:
                               a document closed by the provider then costs one
                               link instead of the tunnel. Both peers need the
                               same set.
      --url-file=<path>        Read documents from a file, one per line
                               ('#' comments allowed). Easier than ten flags.
      --maxToken=<token>       MAX auth token (--transport=oneme).
      --maxUid=<uid>           MAX user id   (--transport=oneme).

INBOUND  (only with --role=client)
  -i, --inbound=tun            utun (macOS) / NEPacketTunnel (iOS). Default on macOS.
  -i, --inbound=socks5         SOCKS5 + gVisor. Default on other platforms.
  -s, --socks5=<addr>          SOCKS5 listen address (default :1080).
      --dns=<ip>               Resolver for --inbound=tun (macOS). UDP queries are
                               re-issued as DNS-over-TCP through the tunnel.

MODE  (only with --role=exit)
  -m, --mode=l3                Packet forwarding (SNAT/DNAT). Default.
  -m, --mode=l4                Stream proxy (TCP termination + re-dial).
  -l, --local-ip=<ip>          Egress IP for SNAT. Auto-detected.

TRANSPORT MODIFIERS
  -c, --codec=batched          zstd + coalescing. Default.
  -c, --codec=legacy           Per-packet LZ4. A/B only.
      --encryption-key-file=<path>
                               AES-256-GCM wrapper. Both peers must share the same key.

BENCHMARK  (only with --role=bench-*)
      --bench-bytes=<MB>       MB to push (bench-send).
      --bench-compressible     Repetitive payload (bench-send).

LOGGING
  -d, --debug                  Verbose per-packet logging.

DEPRECATED (removed in v2)
  -client, -exit-node      -> --role=client|exit
  -tun, -socks5-mode       -> --inbound=tun|socks5
  -legacy                  -> --codec=legacy
  -bench-send, -bench-sink -> --role=bench-send|bench-sink
`)
	}

	os.Args = expandShortFlags(os.Args)
	flag.Parse()

	// Map deprecated flags to their new counterparts. New flags win over
	// deprecated ones if both are supplied.
	roleSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "role" {
			roleSet = true
		}
	})
	if !roleSet {
		if *depClient {
			log.Printf("warning: -client is deprecated, use --role=client")
			*role = roleClient
		}
		if *depExit {
			log.Printf("warning: -exit-node is deprecated, use --role=exit")
			*role = roleExit
		}
	}
	if *depTun {
		log.Printf("warning: -tun is deprecated, use --inbound=tun")
		*inbound = inboundTUN
	}
	if *depSocks5Mode {
		log.Printf("warning: -socks5-mode is deprecated, use --inbound=socks5")
		*inbound = inboundSOCKS5
	}
	if *depLegacy {
		log.Printf("warning: -legacy is deprecated, use --codec=legacy")
		*codec = codecLegacy
	}
	if *depBenchSend > 0 {
		log.Printf("warning: -bench-send is deprecated, use --role=bench-send --bench-bytes=N")
		*role = roleBenchSend
		*benchBytes = *depBenchSend
	}
	if *depBenchSink {
		log.Printf("warning: -bench-sink is deprecated, use --role=bench-sink")
		*role = roleBenchSink
	}

	// Platform defaults. The recommended client path is utun on macOS and
	// SOCKS5 everywhere else (see README for details).
	if *inbound == "" {
		if runtime.GOOS == "darwin" {
			*inbound = inboundTUN
		} else {
			*inbound = inboundSOCKS5
		}
	}
	if *mode == "" {
		*mode = "l3"
	}

	if *urlFile != "" {
		content, err := os.ReadFile(*urlFile)
		if err != nil {
			log.Fatalf("--url-file: %v", err)
		}
		found := parseDocumentList(string(content))
		if len(found) == 0 {
			log.Fatalf("--url-file %s: no document URLs found", *urlFile)
		}
		docURLs = append(docURLs, found...)
		log.Printf("Documents from %s: %d", *urlFile, len(found))
	}
	if len(docURLs) == 0 {
		docURLs = docURLList{"http://#"}
	}
	globalDocUrl = docURLs[0]

	if *codec != codecBatched && *codec != codecLegacy {
		log.Fatalf("--codec: unknown value %q (want batched|legacy)", *codec)
	}

	switch *role {
	case roleClient:
		if *inbound != inboundTUN && *inbound != inboundSOCKS5 {
			log.Fatalf("--role=client: unknown --inbound=%q (want tun|socks5)", *inbound)
		}
	case roleExit:
		if *mode != "l3" && *mode != "l4" {
			log.Fatalf("--role=exit: unknown --mode=%q (want l3|l4)", *mode)
		}
	case roleBenchSend, roleBenchSink:
		// No ingress or exit mode.
	default:
		log.Fatalf("unknown --role=%q (want client|exit|bench-send|bench-sink)", *role)
	}

	// Warn when the exit runs on l4 (gVisor): it works everywhere but is
	// slower than l3 (SNAT/DNAT, Linux only, needs root + iptables).
	if *role == roleExit && *mode == "l4" {
		log.Printf("warning: exit on l4 (gVisor). l3 is faster on Linux with root.")
	}

	exitMode, err := tunnel.ParseExitMode(*mode)
	if err != nil {
		log.Fatalf("--mode: %v", err)
	}

	// The exit node often runs on a tiny VPS; keep the heap tight under load
	// (GC aggressively). Set GOMEMLIMIT in the environment for a hard soft-cap.
	if *role == roleExit {
		godebug.SetGCPercent(20)
	}

	if *debug {
		utils.EnableDebug()
	}

	log.Printf("=== Universal Bypass Tool ===")
	log.Printf("Role: %s", *role)
	log.Printf("Transport: %s", *transportType)
	if *role == roleClient {
		log.Printf("Inbound: %s", *inbound)
	}
	if *role == roleExit {
		log.Printf("Exit mode: %s", exitMode.String())
	}

	config := transport.DefaultConfig()

	// One link per document. The oneme transport is driven by credentials
	// rather than a URL, so it has nothing to bond.
	buildLink := func(docURL string) transport.Transport {
		switch *transportType {
		case "vyandex":
			return yandex.NewYandexVolgaTransport(docURL, config)
		case "yandex":
			return yandex.NewYandexDocsTransport(docURL, config)
		case "oneme":
			uidint, _ := strconv.ParseInt(maxUid, 10, 64)
			return oneme.NewOneMeTransport(*role == roleExit, maxToken, uidint, config)
		case "cupsonline":
			return cupsonline.NewCupsonlineTransport(docURL, config, *role != roleExit)
		case "mailru":
			return mailru.NewMailruDocsTransport(docURL, config)
		case "jitsi":
			return jitsi.NewJitsiTransport(docURL, config)
		default:
			log.Fatalf("Unknown transport type: %s", *transportType)
			return nil
		}
	}

	if *transportType == "oneme" && len(docURLs) > 1 {
		log.Fatalf("--transport=oneme takes credentials, not documents: cannot bond %d URLs", len(docURLs))
	}

	// Held separately: the bond gets wrapped by the codec and the encryption
	// layers, so by the time anything else sees the transport it is an
	// *EncryptedTransport and a type assertion for the bond finds nothing.
	var bond *transport.BondedTransport

	var inner transport.Transport
	if len(docURLs) == 1 {
		inner = buildLink(docURLs[0])
	} else {
		links := make([]transport.Transport, 0, len(docURLs))
		for i, u := range docURLs {
			link := buildLink(u)
			// Name the link by position, never by URL: a failure has to say
			// which document dropped, and the URL is effectively the channel's
			// shared secret — it does not belong in a log that gets pasted
			// into a chat or an issue.
			if l, ok := link.(transport.Labeler); ok {
				l.SetLabel(fmt.Sprintf("документ %d из %d", i+1, len(docURLs)))
			}
			links = append(links, link)
		}
		bonded, err := transport.NewBondedTransport(links)
		if err != nil {
			log.Fatalf("bond documents: %v", err)
		}
		bond = bonded
		// Sessions opened together expire together: the provider ends each
		// after a fixed lifetime (61s on Mail.ru, measured across 64 of them),
		// so links started in the same second also renew in the same second,
		// and a renewal that does not make it takes several of them down at
		// once. The offset is applied to each link's first connection, where
		// no traffic is flowing yet. Five seconds comfortably exceeds how long
		// a connection takes, which is all the spacing needs to do.
		bonded.StaggerLinks(5 * time.Second)
		inner = bonded
		log.Printf("Bonded channel: %d documents", len(links))
	}

	// App-layer codec, outermost. Default is the new batching+zstd layer;
	// --codec=legacy selects the old per-packet LZ4 path so the two can be
	// compared over the same channel. Client and exit node must use the same one.
	switch *codec {
	case codecBatched:
		log.Printf("Codec: batched (zstd + coalescing)")
		inner = transport.NewBatchedTransport(inner)
	case codecLegacy:
		log.Printf("Codec: legacy (per-packet LZ4, no batching)")
		inner = transport.NewCompressedTransport(inner)
	}

	// Optional AES-256-GCM encryption sits closest to the raw transport, so on
	// send we batch/compress first and encrypt the result (ciphertext would not
	// compress). Both peers must use the same secret.
	if *encryptionKeyFile != "" {
		secretBytes, err := os.ReadFile(*encryptionKeyFile)
		if err != nil {
			log.Fatalf("Read encryption key file: %v", err)
		}
		context := encryptionContext(*transportType, docURLs)
		encrypted, err := transport.NewEncryptedTransport(inner, strings.TrimSpace(string(secretBytes)), context, *role == roleExit)
		if err != nil {
			log.Fatalf("Configure encrypted transport: %v", err)
		}
		inner = encrypted
		log.Printf("Transport encryption: AES-256-GCM enabled")
	}

	trans := inner

	// Benchmark modes run the transport directly with no tunnel / raw socket,
	// so they never touch the host network.
	if *role == roleBenchSend {
		if *benchBytes <= 0 {
			log.Fatalf("--role=bench-send requires --bench-bytes=<MB>")
		}
		runBenchSend(trans, *benchBytes, *benchCompressible)
		return
	}
	if *role == roleBenchSink {
		runBenchSink(trans)
		return
	}

	if err := trans.Start(); err != nil {
		log.Fatalf("Failed to start transport: %v", err)
	}

	switch *role {
	case roleExit:
		runExit(trans, bond, exitMode)
	case roleClient:
		runClient(trans, bond, *inbound, *socksAddr, *dnsServer, exitMode)
	default:
		log.Fatalf("unhandled role %q", *role)
	}
}

// encryptionContext derives the key-derivation context both peers must agree
// on. With a single document it is that URL, unchanged from before bonding
// existed. With several, the documents are sorted first so the two sides agree
// even when listed in a different order — links pair up by identity, not by
// position. A transport with no document falls back to its own name.
// sharesDocuments says whether both peers of a transport hold the same list of
// documents. It decides what the encryption context may be built from.
//
// For Mail.ru and the Yandex transports the operator gives both sides the same
// links, so the links identify the channel and make a good salt. cups.online is
// the other kind: the exit node creates its rooms at startup and the client is
// handed them afterwards, so the two sides never hold the same thing — the exit
// has no list at all and the client has the packed one. A context built from
// urls therefore cannot match, by construction, and each peer derives a key the
// other cannot use. oneme is driven by credentials and has the same problem.
var sharesDocuments = map[string]bool{
	"mailru": true, "yandex": true, "vyandex": true, "jitsi": true,
	"cupsonline": false, "oneme": false,
}

// encryptionContext returns the salt both peers must agree on.
//
// It is only a salt — the secrecy is in the shared key file — but the two sides
// must compute it identically or neither can read the other.
func encryptionContext(transportType string, urls []string) string {
	// Whatever the client was given, a transport whose peers do not share a
	// document list has exactly one thing they both always know: its name.
	if !sharesDocuments[transportType] {
		return transportType
	}
	switch {
	case len(urls) == 1 && urls[0] != "":
		return urls[0]
	case len(urls) > 1:
		sorted := append([]string(nil), urls...)
		sort.Strings(sorted)
		return strings.Join(sorted, "|")
	default:
		return transportType
	}
}

func runExit(trans transport.Transport, bond *transport.BondedTransport, exitMode tunnel.ExitMode) {
	ex, err := tunnel.NewExitNode(trans, exitMode.String())
	if err != nil {
		log.Fatalf("exit node: %v", err)
	}
	log.Printf("Running as EXIT NODE (mode=%s)", ex.Mode())

	// The node needs this more than the client does: it runs unattended, and a
	// bond quietly masks individual documents dying. Without it the journal
	// would stay silent while links disappeared one by one.
	go watchTransportHealth(trans, bond)
	if err := ex.Start(); err != nil {
		log.Fatalf("exit start: %v", err)
	}

	// L3 SNAT rewrites source IPs; the kernel sees return packets for
	// connections it never opened and emits RST, tearing them down.
	// The operator must drop outbound RSTs matching the egress IP.
	if exitMode == tunnel.ExitModeL3 {
		if localIP != "" {
			log.Printf("! Run: sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -s %s -j DROP", localIP)
		} else {
			log.Printf("! Kernel RSTs would tear down tunnel connections. Prefer a scoped rule:")
			log.Printf("!   assign a dedicated alias IP, run with --local-ip <ip>, then:")
			log.Printf("!   sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -s <ip> -j DROP")
			log.Printf("! Host-wide fallback (drops ALL outbound RST; makes closed ports look filtered):")
			log.Printf("!   sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -j DROP")
		}
	}

	select {}
}

func runClient(trans transport.Transport, bond *transport.BondedTransport, inbound, socksAddr, dnsServer string, exitMode tunnel.ExitMode) {
	switch inbound {
	case inboundTUN:
		runClientTUN(trans, bond, dnsServer)
	case inboundSOCKS5:
		// Explicit opt-in to the legacy SOCKS5+gVisor client. Kept as a fallback
		// for platforms without a tun client (see README).
		log.Printf("Running as CLIENT (SOCKS5 on %s, legacy gVisor path)", socksAddr)
		tun := tunnel.NewTCPTunnelMode(trans, false, exitMode)
		socks5Server := socks5.NewSOCKS5Server(socksAddr, tun)
		log.Fatal(socks5Server.Start())
	default:
		log.Fatalf("--inbound: unknown value %q (want tun|socks5)", inbound)
	}
}

// watchTransportHealth reports when the channel underneath the tunnel goes
// away and comes back.
//
// The tunnel itself keeps running across a reconnect — the utun interface and
// the routes stay up — so without this nothing distinguishes "carrying
// traffic" from "silently carrying nothing", and the UI goes on claiming the
// tunnel is fine while it is not.
func watchTransportHealth(trans transport.Transport, bond *transport.BondedTransport) {
	// Poll often enough to catch a transition, but only report an outage that
	// outlasts a reconnect. A dropped channel is usually back in under a
	// second, and announcing every blip would cry wolf — while a three-second
	// poll was so coarse that a blip could be reported as an outage and its
	// recovery noticed long after the fact.
	const poll = 500 * time.Millisecond
	const reportAfter = 2 * time.Second

	// A bond hides individual failures on purpose: while one link is up, the
	// transport never reports itself down. That is exactly what keeps traffic
	// flowing, and it would otherwise leave the operator blind — four of five
	// documents could be gone with nothing said. So report the link count as
	// well, on every change, through the ordinary log rather than the debug
	// one: it costs a line an hour at worst, and the alternative is enabling
	// per-packet logging just to learn something operational.
	bonded := bond != nil
	lastUp := -1
	if bonded {
		lastUp = bond.ConnectedLinks()
		log.Printf("Bonded links: %d of %d up", lastUp, bond.Len())
	}

	reported := false
	var downSince time.Time

	for {
		time.Sleep(poll)

		if bonded {
			if up := bond.ConnectedLinks(); up != lastUp {
				log.Printf("Bonded links: %d of %d up", up, bond.Len())
				lastUp = up
			}
		}

		if trans.IsConnected() {
			if reported {
				log.Printf("Transport reconnected")
				reported = false
			}
			downSince = time.Time{}
			continue
		}
		if downSince.IsZero() {
			downSince = time.Now()
		}
		if !reported && time.Since(downSince) >= reportAfter {
			log.Printf("Transport disconnected, reconnecting")
			reported = true
		}
	}
}

func runClientTUN(trans transport.Transport, bond *transport.BondedTransport, dnsServer string) {
	tc, err := NewTUNClient(trans, 1280)
	if err != nil {
		log.Fatalf("utun: %v", err)
	}
	log.Printf("utun interface: %s", tc.Name())

	// Save the CURRENT default (which may be another VPN's utun) so
	// we can restore it on exit no matter what.
	if err := tc.SaveDefault(); err != nil {
		log.Fatalf("save default route: %v", err)
	}
	if err := tc.SetupInterface(); err != nil {
		log.Fatalf("setup utun (need sudo): %v", err)
	}
	log.Printf("utun up; bypass gateway is %s", tc.Gateway())

	watcher := NewSocketWatcher(tc.Gateway(), func() {
		log.Printf("Socket set stable; taking default route into the tunnel")
		if err := tc.ConfigureDefault(); err != nil {
			// Half-configured is the worst outcome: utun is up but carries
			// nothing, and staying alive would strand the machine there. Undo
			// and exit so the supervisor sees a failure.
			log.Printf("configure default route: %v", err)
			tc.Close()
			tc.RestoreDefault()
			os.Exit(1)
		}
		tc.Start()

		// Only now, with the tunnel actually carrying traffic, repoint the
		// resolver. Doing it earlier would aim the OS at a resolver reachable
		// only through a tunnel that is not up yet.
		if dnsServer != "" {
			if err := tc.SetSystemDNS(dnsServer); err != nil {
				log.Printf("warning: could not set system DNS to %s: %v", dnsServer, err)
			} else {
				log.Printf("system DNS set to %s (restored on exit)", dnsServer)
			}
		}
		log.Printf("Tunnel active")
		go watchTransportHealth(trans, bond)
	})
	watcher.SetProtected(tc.IsProtected)
	watcher.Start(2 * time.Second)

	sigCh := make(chan os.Signal, 1)
	notifySignals(sigCh)
	<-sigCh
	watcher.Stop()
	log.Printf("Shutting down, restoring default route...")
	if err := tc.Close(); err != nil {
		log.Printf("cleanup warning: %v", err)
	}
	tc.RestoreDefault()
	log.Printf("Shutdown complete")
	os.Exit(0)
}

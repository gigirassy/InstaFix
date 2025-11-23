package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"instafix/handlers"
	scraper "instafix/handlers/scraper"
	"instafix/utils"
	"instafix/views"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/net/proxy"
)

func init() {
	// Create static folder if not exists
	_ = os.Mkdir("static", 0755)
}

func main() {
	listenAddr := flag.String("listen", "0.0.0.0:3000", "Address to listen on")
	gridCacheMaxFlag := flag.String("grid-cache-entries", "1024", "Maximum number of grid images to cache")
	remoteScraperAddr := flag.String("remote-scraper", "", "Remote scraper address (https://github.com/Wikidepia/InstaFix-remote-scraper)")
	videoProxyAddr := flag.String("video-proxy-addr", "", "Video proxy address (https://github.com/Wikidepia/InstaFix-proxy)")
	flag.Parse()

	// Initialize remote scraper
	if *remoteScraperAddr != "" {
		if !strings.HasPrefix(*remoteScraperAddr, "http") {
			panic("Remote scraper address must start with http:// or https://")
		}
		scraper.RemoteScraperAddr = *remoteScraperAddr
	}

	// Initialize video proxy
	if *videoProxyAddr != "" {
		if !strings.HasPrefix(*videoProxyAddr, "http") {
			panic("Video proxy address must start with http:// or https://")
		}
		handlers.VideoProxyAddr = *videoProxyAddr
		if !strings.HasSuffix(handlers.VideoProxyAddr, "/") {
			handlers.VideoProxyAddr += "/"
		}
	}

	// Initialize logging
	slog.SetLogLoggerLevel(slog.LevelError)

	// Initialize SOCKS proxy pool (if configured)
	if err := initSocksProxyPoolFromEnv("SOCKS_PROXIES"); err != nil {
		slog.Error("Failed to initialize SOCKS proxy pool", "err", err)
	}

	// Initialize LRU
	gridCacheMax, err := strconv.Atoi(*gridCacheMaxFlag)
	if err != nil || gridCacheMax <= 0 {
		panic(err)
	}
	scraper.InitLRU(gridCacheMax)

	// Initialize cache / DB
	scraper.InitDB()
	defer scraper.DB.Close()

	// Evict cache every 5 minutes
	go func() {
		for {
			evictCache()
			time.Sleep(5 * time.Minute)
		}
	}()

	// pprof endpoint
	go func() {
		_ = http.ListenAndServe("localhost:6060", nil)
	}()

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.StripSlashes)

	r.Get("/tv/{postID}", handlers.Embed)
	r.Get("/reel/{postID}", handlers.Embed)
	r.Get("/reels/{postID}", handlers.Embed)
	r.Get("/stories/{username}/{postID}", handlers.Embed)
	r.Get("/p/{postID}", handlers.Embed)
	r.Get("/p/{postID}/{mediaNum}", handlers.Embed)

	r.Get("/{username}/p/{postID}", handlers.Embed)
	r.Get("/{username}/p/{postID}/{mediaNum}", handlers.Embed)
	r.Get("/{username}/reel/{postID}", handlers.Embed)
	r.Get("/share/{postID}", handlers.Embed)

	r.Get("/images/{postID}/{mediaNum}", handlers.Images)
	r.Get("/videos/{postID}/{mediaNum}", handlers.Videos)
	r.Get("/grid/{postID}", handlers.Grid)
	r.Get("/oembed", handlers.OEmbed)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		views.Home(w)
	})

	println("Starting up on", *listenAddr)
	if err := http.ListenAndServe(*listenAddr, r); err != nil {
		slog.Error("Failed to listen", "err", err)
	}
}

// initSocksProxyPoolFromEnv reads comma-separated SOCKS5 URIs from the provided env var (e.g. "socks5://user:pass@ip:1080")
// and installs a custom http.Transport that round-robins connections through them. If the env var is empty or no valid proxies
// are parsed, this function leaves the default client/transport alone.
func initSocksProxyPoolFromEnv(envVar string) error {
	raw := strings.TrimSpace(os.Getenv(envVar))
	if raw == "" {
		// nothing to do
		slog.Info("SOCKS proxy pool: none configured (env var empty)", "env", envVar)
		return nil
	}

	parts := strings.Split(raw, ",")
	var dialers []proxy.Dialer
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		u, err := url.Parse(p)
		if err != nil {
			slog.Error("Skipping invalid SOCKS URI", "uri", p, "err", err)
			continue
		}
		// Accept socks5 or socks scheme
		if u.Scheme != "socks5" && u.Scheme != "socks5h" && u.Scheme != "socks" {
			slog.Error("Skipping unsupported scheme (use socks5)", "uri", p)
			continue
		}
		host := u.Host
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{
				User:     u.User.Username(),
				Password: pw,
			}
		}

		d, err := proxy.SOCKS5("tcp", host, auth, proxy.Direct)
		if err != nil {
			slog.Error("Failed to create SOCKS5 dialer", "uri", p, "err", err)
			continue
		}
		dialers = append(dialers, d)
	}

	if len(dialers) == 0 {
		slog.Info("SOCKS proxy pool: no valid proxies parsed, using direct connections")
		return nil
	}

	var next uint64

	// DialContext wrapper that round-robins between dialers and fails over on dial errors.
	dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		// quick direct-dial fallback if no dialers (shouldn't happen here)
		if len(dialers) == 0 {
			d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
			return d.DialContext(ctx, network, addr)
		}

		// choose starting index via atomic counter. parentheses and spacing are important to avoid parser ambiguity
		start := int((atomic.AddUint64(&next, 1) - 1)) % len(dialers)

		var lastErr error
		// try each dialer once, starting at start (simple failover)
		for i := 0; i < len(dialers); i++ {
			idx := (start + i) % len(dialers)
			d := dialers[idx]
			// x/net/proxy Dialer exposes Dial(network, addr) (no context)
			conn, err := d.Dial(network, addr)
			if err == nil {
				// honor context cancellation if already done
				select {
				case <-ctx.Done():
					_ = conn.Close()
					return nil, ctx.Err()
				default:
					return conn, nil
				}
			}
			lastErr = err
			// otherwise try next proxy
		}
		// all failed
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, context.DeadlineExceeded
	}

	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	// replace default transport & client so existing http.* calls will use the proxy pool
	http.DefaultTransport = transport
	http.DefaultClient = &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	slog.Info("SOCKS proxy pool initialized", "proxies", len(dialers))
	return nil
}

// Remove cache from Pebble if already expired
func evictCache() {
	curTime := time.Now().UnixNano()
	err := scraper.DB.Batch(func(tx *bolt.Tx) error {
		ttlBucket := tx.Bucket([]byte("ttl"))
		if ttlBucket == nil {
			return nil
		}
		dataBucket := tx.Bucket([]byte("data"))
		if dataBucket == nil {
			return nil
		}
		c := ttlBucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if n, err := strconv.ParseInt(utils.B2S(k), 10, 64); err == nil {
				if n < curTime {
					ttlBucket.Delete(k)
					dataBucket.Delete(v)
				}
			} else {
				slog.Error("Failed to parse expire timestamp in cache", "err", err)
			}
		}
		return nil
	})
	if err != nil {
		slog.Error("Failed to evict cache", "err", err)
	}
}

package proxyd

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
	"github.com/pkg/errors"
)

type ProxydConfig struct {
	ListenAddr     string `envconfig:"PROXYD_LISTEN_ADDR"`
	BaseProxyUrl   string `envconfig:"PROXYD_BASE_PROXY_URL"`
	ProxyApiKey    string `envconfig:"PROXYD_PROXY_API_KEY"`
	CacheTTL       int    `envconfig:"PROXYD_CACHE_TTL_SECS"`
	RestrictOrigin bool   `envconfig:"PROXYD_RESTRICT_ORIGIN"`
	AllowHeaders   string `envconfig:"PROXYD_ALLOW_HEADERS"`
	AllowMethods   string `envconfig:"PROXYD_ALLOW_METHODS"`
}

type Proxyd struct {
	reverseProxy *httputil.ReverseProxy
	cache        Cache
	done         chan struct{}
	config       *ProxydConfig
	templateURL  *url.URL
}

const (
	// default to free/public api
	defaultGeckoApiUrl = "https://api.coingecko.com"
	// max response size to attempt to cache
	maxResponseSize = 1024 * 1024 * 10 // 10MB
	// header name for authenticated requests to gecko
	geckoApiHeaderName = "x-cg-pro-api-key"
	cacheStatusHeader  = "X-Mkt-Cache"
)

func New() *Proxyd {
	proxyConfig := &ProxydConfig{}
	if err := envconfig.Process("", proxyConfig); err != nil {
		panic(fmt.Sprintf("error loading proxyd config: %+v\n", err))
	}

	if proxyConfig.ListenAddr == "" {
		proxyConfig.ListenAddr = ":8080"
	}
	if proxyConfig.BaseProxyUrl == "" {
		proxyConfig.BaseProxyUrl = defaultGeckoApiUrl
	}
	proxyUrl, err := url.Parse(proxyConfig.BaseProxyUrl)
	if err != nil {
		panic(fmt.Sprintf("error parsing base proxy url: %+v\n", err))
	}

	template, err := url.Parse(proxyConfig.BaseProxyUrl)
	if err != nil {
		panic(fmt.Sprintf("error parsing base proxy url: %+v\n", err))
	}

	singleHostReverseProxy := httputil.NewSingleHostReverseProxy(proxyUrl)
	proxyd := &Proxyd{
		reverseProxy: singleHostReverseProxy,
		cache:        &MemoryCache{data: make(map[string]*CachedResponse, 1024), ttl: time.Duration(proxyConfig.CacheTTL) * time.Second},
		done:         make(chan struct{}),
		config:       proxyConfig,
		templateURL:  template,
	}

	// must nil out Director
	proxyd.reverseProxy.Director = proxyd.director
	// proxyd.reverseProxy.Rewrite = proxyd.rewrite
	proxyd.reverseProxy.ModifyResponse = proxyd.modifyResponse
	proxyd.reverseProxy.ErrorHandler = proxyd.errorHandler

	// bind / to cache handler
	http.HandleFunc("/", proxyd.cachingHandler)

	go func() {
		fmt.Printf("starting proxyd of %s on %s\n", proxyConfig.BaseProxyUrl, proxyConfig.ListenAddr)
		if err := http.ListenAndServe(proxyConfig.ListenAddr, nil); err != nil {
			panic(fmt.Sprintf("error serving http: %v\n", err))
		}
	}()

	return proxyd
}

func (p *Proxyd) errorHandler(w http.ResponseWriter, r *http.Request, srcErr error) {
	fmt.Printf("error proxying request: %+v\n", srcErr)
	http.Error(w, "error proxying request", http.StatusInternalServerError)
}

func (p *Proxyd) director(r *http.Request) {
	r.URL.Scheme = p.templateURL.Scheme
	r.URL.Host = p.templateURL.Host
	r.Host = p.templateURL.Host
	r.RemoteAddr = ""

	if p.config.ProxyApiKey != "" {
		r.Header.Set(geckoApiHeaderName, p.config.ProxyApiKey)
	}
}

func (p *Proxyd) cachingHandler(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/api/v3/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	cachedResponse, ok := p.cache.Get(r)
	if ok {
		for k, v := range cachedResponse.headers {
			w.Header()[k] = v
		}
		// set our cache status header
		w.Header()[cacheStatusHeader] = []string{"HIT"}

		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(cachedResponse.body); err != nil {
			fmt.Printf("error writing cache hit response: %+v\n", err)
		}
		fmt.Printf("request: %s served from cache\n", r.URL.Path)
	} else {
		p.reverseProxy.ServeHTTP(w, r)
		fmt.Printf("request: %s proxied\n", r.URL.Path)
	}
}

func (p *Proxyd) modifyResponse(r *http.Response) error {
	// Check the ContentLength header
	if r.ContentLength > maxResponseSize {
		fmt.Printf("warn: response too large: %d\n", r.ContentLength)
		// TODO Handle large responses
		return nil
	}

	// Use a buffer to store the response
	buf, err := func() (bytes.Buffer, error) {
		defer r.Body.Close()
		var buf bytes.Buffer
		_, err := io.Copy(&buf, r.Body)
		return buf, err
	}()

	if err != nil {
		return errors.Wrapf(err, "error reading proxied response body")
	}

	responseBody := buf.Bytes()

	// replace the body client will see
	r.Body = io.NopCloser(&buf)

	p.accessControl(r)
	r.Header.Set("Cache-Control", "public, max-age=30")
	r.Header.Set("Age", "0")
	r.Header.Del("Alternate-Protocol")

	// create a cache key, store the response
	cacheKey := cacheKey(r.Request)
	cachedResponse := &CachedResponse{
		body:    responseBody,
		headers: r.Header,
	}

	if err = p.cache.Put(cacheKey, cachedResponse); err != nil {
		return errors.Wrapf(err, "error storing response in cache")
	}

	return nil
}

func (p *Proxyd) accessControl(r *http.Response) {
	// remove existing access-control* headers
	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "access-control") {
			r.Header.Del(k)
		}
	}

	if p.config.RestrictOrigin {
		if strings.HasSuffix(r.Request.Header.Get("Origin"), "shapeshift.com") {
			r.Header.Set("Access-Control-Allow-Origin", r.Request.Header.Get("Origin"))
		} else if strings.HasPrefix(r.Request.Header.Get("Origin"), "http://localhost:") {
			r.Header.Set("Access-Control-Allow-Origin", r.Request.Header.Get("Origin"))
		} else {
			r.Header.Set("Access-Control-Allow-Origin", "app.shapeshift.com")
		}
	} else {
		r.Header.Set("Access-Control-Allow-Origin", "*")
	}

	if p.config.AllowMethods != "" {
		r.Header.Set("Access-Control-Allow-Methods", p.config.AllowMethods)
	} else {
		r.Header.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	}

	if p.config.AllowHeaders != "" {
		r.Header.Set("Access-Control-Allow-Headers", p.config.AllowHeaders)
	} else {
		r.Header.Set("Access-Control-Allow-Headers", "Origin, Content-Type, X-Requested-With, Accept")
	}
}

func (p *Proxyd) Done() chan struct{} {
	return p.done
}

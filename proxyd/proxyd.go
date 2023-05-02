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
	BaseProxyUrl string `envconfig:"PROXYD_BASE_PROXY_URL"`
	ProxyApiKey  string `envconfig:"PROXYD_PROXY_API_KEY"`
	CacheTTL     int    `envconfig:"PROXYD_CACHE_TTL_SECS"`
	AllowOrigin  string `envconfig:"PROXYD_ALLOW_ORIGIN"`
	AllowHeaders string `envconfig:"PROXYD_ALLOW_HEADERS"`
	AllowMethods string `envconfig:"PROXYD_ALLOW_METHODS"`

	// local dev only
	// InsecureSkipVerify bool `envconfig:"PROXYD_INSECURE_SKIP_VERIFY"`
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

	// if proxyConfig.InsecureSkipVerify {
	// 	proxyd.reverseProxy.Transport = &http.Transport{
	// 		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	// 	}
	// }

	// must nil out Director
	proxyd.reverseProxy.Director = nil
	proxyd.reverseProxy.Rewrite = proxyd.rewrite
	proxyd.reverseProxy.ModifyResponse = proxyd.modifyResponse

	// bind / to cache handler
	http.HandleFunc("/", proxyd.cachingHandler)

	go func() {
		if err := http.ListenAndServe(":1137", nil); err != nil {
			panic(fmt.Sprintf("error serving http: %v\n", err))
		}
	}()

	return proxyd
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

	if p.config.AllowOrigin != "" {
		r.Header.Set("Access-Control-Allow-Origin", p.config.AllowOrigin)
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

func (p *Proxyd) rewrite(r *httputil.ProxyRequest) {
	r.Out.URL.Scheme = p.templateURL.Scheme
	r.Out.URL.Host = p.templateURL.Host
	r.Out.Host = p.templateURL.Host
	r.Out.RemoteAddr = ""

	if p.config.ProxyApiKey != "" {
		r.Out.Header.Set(geckoApiHeaderName, p.config.ProxyApiKey)
	}
}

func (p *Proxyd) cachingHandler(w http.ResponseWriter, r *http.Request) {
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
	} else {
		p.reverseProxy.ServeHTTP(w, r)
	}
}

func (p *Proxyd) Done() chan struct{} {
	return p.done
}

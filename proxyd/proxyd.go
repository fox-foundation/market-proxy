package proxyd

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/pkg/errors"
)

type Proxyd struct {
	reverseProxy *httputil.ReverseProxy
	cache        Cache
	done         chan struct{}
}

func New() *Proxyd {
	proxyUrl, err := url.Parse("https://api.coingecko.com")
	if err != nil {
		panic(err)
	}

	singleHostReverseProxy := httputil.NewSingleHostReverseProxy(proxyUrl)
	proxyd := &Proxyd{
		reverseProxy: singleHostReverseProxy,
		cache:        &MemoryCache{data: make(map[string]*CachedResponse, 1024)},
		done:         make(chan struct{}),
	}

	// TODO: Remove InsecureSkipVerify configure tls
	proxyd.reverseProxy.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

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

const maxResponseSize = 1024 * 1024 * 10 // 10MB

func (p *Proxyd) modifyResponse(r *http.Response) error {
	// Check the ContentLength header
	if r.ContentLength > maxResponseSize {
		fmt.Printf("warn: response too large: %d\n", r.ContentLength)
		// TODO Handle large responses
		return nil
	}

	// Use a buffer to store the response
	var buf bytes.Buffer
	_, err := io.Copy(&buf, r.Body)
	if err != nil {
		return errors.Wrapf(err, "error reading proxied response body")
	}

	// Store the response body in a variable
	responseBody := buf.Bytes()

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

func (p *Proxyd) rewrite(r *httputil.ProxyRequest) {
	r.Out.URL.Scheme = "https"
	r.Out.URL.Host = "api.coingecko.com"
	r.Out.Host = r.Out.URL.Host
	r.Out.RemoteAddr = ""
}

func (p *Proxyd) cachingHandler(w http.ResponseWriter, r *http.Request) {
	cachedResponse, ok := p.cache.Get(r)
	if ok {
		for k, v := range cachedResponse.headers {
			w.Header()[k] = v
		}
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(cachedResponse.body); err != nil {
			fmt.Printf("error writing cache hit response: %+v\n", err)
		}
	} else {
		p.reverseProxy.ServeHTTP(w, r)
	}
}

func cacheKey(r *http.Request) string {
	reqUrl := r.URL.Path
	reqParams := r.URL.Query()
	return fmt.Sprintf("%s?%s", reqUrl, reqParams.Encode())
}

func (p *Proxyd) Done() chan struct{} {
	return p.done
}

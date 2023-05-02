package proxyd

import (
	"fmt"
	"net/http"
)

type CachedResponse struct {
	body    []byte
	headers http.Header
}

type Cache interface {
	Get(r *http.Request) (*CachedResponse, bool)
	Put(key string, value *CachedResponse) error
}

// naive in-memory cache
type MemoryCache struct {
	data map[string]*CachedResponse
}

func (m *MemoryCache) Get(r *http.Request) (*CachedResponse, bool) {
	cacheKey := cacheKey(r)
	if val, ok := m.data[cacheKey]; ok {
		return val, true
	}

	return nil, false
}

func (m *MemoryCache) Put(key string, value *CachedResponse) error {
	m.data[key] = value
	return nil
}

func cacheKey(r *http.Request) string {
	reqUrl := r.URL.Path
	reqParams := r.URL.Query()
	return fmt.Sprintf("%s?%s", reqUrl, reqParams.Encode())
}

package main

import (
	"os"
	"os/signal"
	"errors"
	"net/http"
	"encoding/json"
	"time"
	"context"
	"log"
	"fmt"
	"net/url"
	"sync"

	"github.com/joho/godotenv"
)

var cache = &Cache{TTL: 1*time.Hour}

func main() {
	if err := godotenv.Load(".env"); err != nil {
		_ = godotenv.Load("/etc/fedinfo/env")
	}

	cacheFile := os.Getenv("CACHE_FILE")
	log.Printf("populating cache from %s", cacheFile)

	fd, err := os.Open(cacheFile)
	if err != nil {
		log.Printf("failed to open cache file: %v", err)
	} else {
		var cacheData map[string]Software
		if err := json.NewDecoder(fd).Decode(&cacheData); err != nil {
			log.Printf("failed to populate cache: %v", err)
		} else {
			cache.Data = cacheData
		}
		fd.Close()
	}
	defer func() {
		fd, err := os.Create(cacheFile)
		if err != nil {
			log.Printf("failed to open cache file for writing: %v", err)
		} else {
			if err := json.NewEncoder(fd).Encode(cache.Data); err != nil {
				log.Printf("failed to write out cache: %v", err)
			}
		}
	}()

	listen := os.Getenv("LISTEN")
	log.Printf("listening on %s", listen)

	mux := http.NewServeMux()
	mux.Handle("GET /node-info", HandlerWithError(nodeInfoRoute))
	srv := &http.Server{
		Addr: listen,
		Handler: mux,
	}

	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Println(err)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c

	log.Println("interrupt received, stopped accepting requests")
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("error while shutting down server: %v", err)
	}
}

type (
	HandlerWithError func(w http.ResponseWriter, r *http.Request) error
	ErrorResponder interface {
		RespondError(w http.ResponseWriter, r *http.Request) (wasHandled bool)
	}
	ErrMissingParam string
	ErrBadRequest string
)

func (h HandlerWithError) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h(w, r); err != nil {
		if err, ok := err.(ErrorResponder); ok {
			if err.RespondError(w, r) {
				return
			}
		}
		status := http.StatusInternalServerError
		http.Error(w, http.StatusText(status), status)
		log.Printf("unhandled error in http request handler: %v", err)
	}
}

func (e ErrMissingParam) Error() string {
	return fmt.Sprintf("missing mandatory parameter: %s", string(e))
}

func (e ErrMissingParam) RespondError(w http.ResponseWriter, r *http.Request) bool {
	status := http.StatusBadRequest
	http.Error(w, e.Error(), status)
	return true
}

func (e ErrBadRequest) Error() string {
	return string(e)
}

func (e ErrBadRequest) RespondError(w http.ResponseWriter, r *http.Request) bool {
	status := http.StatusBadRequest
	http.Error(w, e.Error(), status)
	return true
}

type (
	WellKnownNodeInfo struct {
		Links []Link `json:"links"`
	}
	Link struct {
		Rel string `json:"rel"`
		Href string `json:"href"`
	}
	NodeInfo struct {
		Domain string `json:"domain"`
		Software Software `json:"software"`
	}
	Software struct {
		Name string `json:"name"`
		Version string `json:"version"`
	}
)

func nodeInfoRoute(w http.ResponseWriter, r *http.Request) error {
	log.Printf("request received: %s", r.URL.Path)
	if err := r.ParseForm(); err != nil {
		return err
	}
	domain := r.Form.Get("domain")
	if domain == "" {
		return ErrMissingParam("domain")
	}
	parsedDomain, err := url.Parse(domain)
	if err != nil {
		return ErrBadRequest(fmt.Sprintf("not an url: %s", domain))
	}
	if parsedDomain.Host == "" {
		domain = parsedDomain.Path // if you don't enter a schema, url.Parse will think the domain is the path
	} else {
		domain = parsedDomain.Host
	}
	queryResponse := NodeInfo{
		Domain: domain,
	}
	if sfw, ok := cache.Get(domain); ok {
		queryResponse.Software = sfw
	} else {
		resp, err := http.Get(fmt.Sprintf("https://%s/.well-known/nodeinfo", domain))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		wk := WellKnownNodeInfo{}
		if err := json.NewDecoder(resp.Body).Decode(&wk); err != nil {
			return err
		}
		var nodeInfoUrl string
		for _, link := range wk.Links {
			switch link.Rel {
			default:
				// continue
			case "http://nodeinfo.diaspora.software/ns/schema/2.0":
				fallthrough
			case "http://nodeinfo.diaspora.software/ns/schema/2.1":
				nodeInfoUrl = link.Href
				break
			}
		}
		if len(nodeInfoUrl) > 0 {
			resp, err := http.Get(nodeInfoUrl)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			var resInfo struct {
				Software Software `json:"software"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&resInfo); err != nil {
				return err
			}
			cache.Set(domain, resInfo.Software)
			queryResponse.Software = resInfo.Software
		}
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(queryResponse); err != nil {
		return err
	}
	return nil
}

type Cache struct {
	TTL time.Duration
	Data map[string]Software
	Age map[string]time.Time
	lock sync.RWMutex
}

func (c *Cache) Get(key string) (sfw Software, foundAndNotStale bool) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.segfaultPrevention()
	if age, ok := c.Age[key]; ok {
		if time.Now().Sub(age) > c.TTL {
			return sfw, false
		}
		sfw, foundAndNotStale = c.Data[key]
		return sfw, foundAndNotStale
	}
	if sfw, ok := c.Data[key]; ok {
		c.Age[key] = time.Now()
		return sfw, true
	}
	return sfw, false
}

func (c *Cache) Set(key string, sfw Software) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.segfaultPrevention()
	c.Data[key] = sfw
	c.Age[key] = time.Now()
}

func (c *Cache) segfaultPrevention() {
	if c.Data == nil {
		c.Data = map[string]Software{}
	}
	if c.Age == nil {
		c.Age = map[string]time.Time{}
	}
}

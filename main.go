package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"golang.org/x/time/rate"
)

const (
	maxRequestBodySize = 1 << 20
	writeWait          = 10 * time.Second
	pongWait           = 60 * time.Second
	pingPeriod         = (pongWait * 9) / 10
	maxMessageSize     = 512 * 1024
)

type WebhookEvent struct {
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	Query      map[string][]string `json:"query"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
	RemoteAddr string              `json:"remote_addr"`
	StatusCode int                 `json:"status_code"`
	ReceivedAt time.Time           `json:"received_at"`
}

type Broadcast struct {
	Token string
	Event WebhookEvent
}

type Client struct {
	conn  *websocket.Conn
	token string
	send  chan []byte
}

type Hub struct {
	mu         sync.RWMutex
	clients    map[*Client]bool
	register   chan *Client
	unregister chan *Client
	broadcast  chan Broadcast
	maxClients int
}

func newHub() *Hub {
	maxClients := 1000
	if maxClientsEnv := os.Getenv("MAX_WS_CLIENTS"); maxClientsEnv != "" {
		if parsed, err := strconv.Atoi(maxClientsEnv); err == nil && parsed > 0 {
			maxClients = parsed
		}
	}
	
	return &Hub{
		clients:    make(map[*Client]bool),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		broadcast:  make(chan Broadcast),
		maxClients: maxClients,
	}
}

func (h *Hub) run() {
	for {
		select {
		case c := <-h.register:
			h.mu.Lock()
			if len(h.clients) >= h.maxClients {
				h.mu.Unlock()
				_ = c.conn.Close()
				continue
			}
			h.clients[c] = true
			h.mu.Unlock()

		case c := <-h.unregister:
			h.removeClient(c)

		case msg := <-h.broadcast:
			payload, err := json.Marshal(msg.Event)
			if err != nil {
				continue
			}

			h.mu.RLock()
			for c := range h.clients {
				if c.token != msg.Token {
					continue
				}
				select {
				case c.send <- payload:
				default:
					go func(cl *Client) {
						h.unregister <- cl
					}(c)
				}
			}
			h.mu.RUnlock()
		}
	}
}

func (h *Hub) removeClient(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	close(c.send)
	_ = c.conn.Close()
}

func (h *Hub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *Hub) canRegister() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients) < h.maxClients
}

var hub = newHub()

var upgrader websocket.Upgrader

func initUpgrader() {
	allowedOrigins := make(map[string]bool)
	
	if originsEnv := os.Getenv("ALLOWED_ORIGINS"); originsEnv != "" {
		origins := strings.Split(originsEnv, ",")
		for _, origin := range origins {
			origin = strings.TrimSpace(origin)
			if origin != "" {
				allowedOrigins[origin] = true
			}
		}
	}
	
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			
			if len(allowedOrigins) > 0 {
				return allowedOrigins[origin]
			}
			
			if origin == "" {
				return false
			}
			
			originURL, err := url.Parse(origin)
			if err != nil {
				return false
			}
			
			requestHost := r.Host
			if !strings.Contains(requestHost, ":") {
				if originURL.Port() != "" {
					requestHost = requestHost + ":" + originURL.Port()
				}
			}
			
			return originURL.Host == requestHost || originURL.Hostname() == strings.Split(requestHost, ":")[0]
		},
	}
}

type limiterEntry struct {
	limiter   *rate.Limiter
	lastUsed  time.Time
}

type IPRateLimiter struct {
	entries     map[string]*limiterEntry
	mu          sync.Mutex
	r           rate.Limit
	b           int
	cleanupInterval time.Duration
	maxIdleTime    time.Duration
	stopCleanup    chan struct{}
}

func NewIPRateLimiter(r rate.Limit, b int) *IPRateLimiter {
	limiter := &IPRateLimiter{
		entries:        make(map[string]*limiterEntry),
		r:              r,
		b:              b,
		cleanupInterval: 5 * time.Minute, 
		maxIdleTime:     15 * time.Minute,
		stopCleanup:     make(chan struct{}),
	}
	
	go limiter.cleanup()
	
	return limiter
}

func (l *IPRateLimiter) getLimiter(ip string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, exists := l.entries[ip]
	if !exists {
		entry = &limiterEntry{
			limiter:  rate.NewLimiter(l.r, l.b),
			lastUsed: time.Now(),
		}
		l.entries[ip] = entry
	} else {
		entry.lastUsed = time.Now()
	}
	return entry.limiter
}

func (l *IPRateLimiter) Allow(ip string) bool {
	return l.getLimiter(ip).Allow()
}

func (l *IPRateLimiter) cleanup() {
	ticker := time.NewTicker(l.cleanupInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			l.cleanupInactive()
		case <-l.stopCleanup:
			return
		}
	}
}

func (l *IPRateLimiter) cleanupInactive() {
	l.mu.Lock()
	defer l.mu.Unlock()
	
	now := time.Now()
	for ip, entry := range l.entries {
		if now.Sub(entry.lastUsed) > l.maxIdleTime {
			delete(l.entries, ip)
		}
	}
}

func (l *IPRateLimiter) Stop() {
	close(l.stopCleanup)
}

var ipLimiter = NewIPRateLimiter(1, 5)

func rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := getIP(r)
		if !ipLimiter.Allow(ip) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func getIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isValidToken(t string) bool {
	_, err := uuid.Parse(t)
	return err == nil
}

func sanitizeHeaders(h http.Header) map[string][]string {
	safe := make(map[string][]string)
	for k, v := range h {
		kl := strings.ToLower(k)
		if strings.HasPrefix(kl, "x-") ||
			kl == "user-agent" ||
			kl == "content-type" ||
			kl == "x-forwarded-for" {
			safe[k] = v
		}
	}
	return safe
}

func (c *Client) readPump() {
	defer func() {
		hub.unregister <- c
	}()

	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			break
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		hub.unregister <- c
	}()

	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}

		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	token := path.Base(r.URL.Path)
	if !isValidToken(token) {
		http.Error(w, "invalid token", http.StatusBadRequest)
		return
	}

	if !hub.canRegister() {
		http.Error(w, "server at capacity", http.StatusServiceUnavailable)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	client := &Client{
		conn:  conn,
		token: token,
		send:  make(chan []byte, 256),
	}

	hub.register <- client

	go client.writePump()
	go client.readPump()
}

func webhookHandler(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/h/")
	if !isValidToken(token) {
		http.Error(w, "invalid token", http.StatusBadRequest)
		return
	}

	defer r.Body.Close()
	limited := io.LimitReader(r.Body, maxRequestBodySize)
	bodyBytes, err := io.ReadAll(limited)
	if err != nil {
		http.Error(w, "unable to read body", http.StatusInternalServerError)
		return
	}

	responseStatus := http.StatusOK

	evt := WebhookEvent{
		Method:     r.Method,
		Path:       r.URL.Path,
		Query:      r.URL.Query(),
		Headers:    sanitizeHeaders(r.Header),
		Body:       string(bodyBytes),
		RemoteAddr: getIP(r),
		StatusCode: responseStatus,
		ReceivedAt: time.Now().UTC(),
	}

	hub.broadcast <- Broadcast{Token: token, Event: evt}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(responseStatus)
	_, _ = w.Write([]byte(`{"status":"received"}`))
}

func createEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token := uuid.NewString()
	http.Redirect(w, r, "/view/"+token, http.StatusSeeOther)
}

func viewPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/view.html")
}

func landing(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/index.html")
}

func main() {
	_ = godotenv.Load()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	initUpgrader()
	go hub.run()

	mux := http.NewServeMux()

	mux.HandleFunc("/", landing)
	mux.HandleFunc("/view/", viewPage)
	mux.HandleFunc("/ws/", wsHandler)
	mux.Handle("/api/create", rateLimitMiddleware(http.HandlerFunc(createEndpoint)))
	mux.Handle("/h/", rateLimitMiddleware(http.HandlerFunc(webhookHandler)))
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir("web/assets"))))

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Println("Listening on :" + port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

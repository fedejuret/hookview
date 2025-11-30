package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
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
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		broadcast:  make(chan Broadcast),
	}
}

func (h *Hub) run() {
	for {
		select {
		case c := <-h.register:
			h.mu.Lock()
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

var hub = newHub()

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type IPRateLimiter struct {
	limiters map[string]*rate.Limiter
	mu       sync.Mutex
	r        rate.Limit
	b        int
}

func NewIPRateLimiter(r rate.Limit, b int) *IPRateLimiter {
	return &IPRateLimiter{
		limiters: make(map[string]*rate.Limiter),
		r:        r,
		b:        b,
	}
}

func (l *IPRateLimiter) getLimiter(ip string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()

	limiter, exists := l.limiters[ip]
	if !exists {
		limiter = rate.NewLimiter(l.r, l.b)
		l.limiters[ip] = limiter
	}
	return limiter
}

func (l *IPRateLimiter) Allow(ip string) bool {
	return l.getLimiter(ip).Allow()
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

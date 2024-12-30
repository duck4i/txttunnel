package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	RequestsPerMinute = 100           // Requests per minute for each IP
	BurstSize         = 10            // Burst size for rate limiter
	CleanupInterval   = 1 * time.Hour // How often to cleanup old rate limiters
)

type RateLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type RateLimiterStore struct {
	sync.RWMutex
	limiters    map[string]*RateLimiter
	cleanupFreq time.Duration
}

var (
	ipLimiters = &RateLimiterStore{
		limiters:    make(map[string]*RateLimiter),
		cleanupFreq: CleanupInterval,
	}

	tunnelLimiters = &RateLimiterStore{
		limiters:    make(map[string]*RateLimiter),
		cleanupFreq: CleanupInterval,
	}
)

func init() {
	go ipLimiters.cleanup()
	go tunnelLimiters.cleanup()
}

func (store *RateLimiterStore) cleanup() {
	for {
		time.Sleep(store.cleanupFreq)

		store.Lock()
		for ip, limiter := range store.limiters {
			if time.Since(limiter.lastSeen) > store.cleanupFreq {
				delete(store.limiters, ip)
			}
		}
		store.Unlock()
	}
}

func (store *RateLimiterStore) getLimiter(key string) *rate.Limiter {
	store.Lock()
	defer store.Unlock()

	limiter, exists := store.limiters[key]
	if !exists {
		limiter = &RateLimiter{
			limiter:  rate.NewLimiter(rate.Every(time.Minute/100), 10),
			lastSeen: time.Now(),
		}
		store.limiters[key] = limiter
	}

	limiter.lastSeen = time.Now()
	return limiter.limiter
}

func withRateLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		if forwardedFor := r.Header.Get("X-Forwarded-For"); forwardedFor != "" {
			ip = forwardedFor
		}

		if !ipLimiters.getLimiter(ip).Allow() {
			http.Error(w, "IP rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		tunnelID := ""
		if r.Method == http.MethodGet {
			tunnelID = r.URL.Query().Get("id")
			if tunnelID == "" {
				tunnelID = r.URL.Query().Get("ID")
			}
		} else if r.Method == http.MethodPost {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				tunnelID = body["id"]
				if tunnelID == "" {
					tunnelID = body["ID"]
				}
				// Restore body for next handler
				jsonBody, _ := json.Marshal(body)
				r.Body = io.NopCloser(bytes.NewBuffer(jsonBody))
			}
		}

		if tunnelID != "" {
			if !tunnelLimiters.getLimiter(tunnelID).Allow() {
				http.Error(w, "Tunnel rate limit exceeded", http.StatusTooManyRequests)
				return
			}
		}

		next(w, r)
	}
}

type Tunnel struct {
	ID          string
	Content     string
	SubChannels map[string]string
}

var tunnels = make(map[string]*Tunnel)
var tunnelsMutex = &sync.Mutex{}
var clients = make(map[string]map[string][]chan string)
var clientsMutex = &sync.Mutex{}

func main() {
	log.Println("Starting server on port 2427")
	http.HandleFunc("/", withCORS(homePage))
	http.HandleFunc("/LICENSE", withCORS(giveLicense))
	http.HandleFunc("/api/v3/tunnel/create", withCORS(withRateLimit(createTunnel)))
	http.HandleFunc("/api/v3/tunnel/stream", withCORS(withRateLimit(streamTunnelContent)))
	http.HandleFunc("/api/v3/tunnel/get", withCORS(withRateLimit(getTunnelContent)))
	http.HandleFunc("/api/v3/tunnel/send", withCORS(withRateLimit(sendToTunnel)))
	log.Fatal(http.ListenAndServe(":2427", nil))
}

func giveLicense(w http.ResponseWriter, r *http.Request) {
	log.Println("Serving LICENSE file")
	http.ServeFile(w, r, "web/LICENSE.txt")
}

func homePage(w http.ResponseWriter, r *http.Request) {
	log.Println("Serving home page")
	http.ServeFile(w, r, "web/index.html")
}

func withCORS(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		handler(w, r)
	}
}

func getTunnelContent(w http.ResponseWriter, r *http.Request) {
	tunnelId := ""
	subChannel := ""
	if r.Method == http.MethodGet {
		tunnelId = r.URL.Query().Get("id")
		subChannel = r.URL.Query().Get("subChannel")
		if r.URL.Query().Get("subchannel") != "" {
			subChannel = r.URL.Query().Get("subchannel")
		}
		if r.URL.Query().Get("ID") != "" {
			tunnelId = r.URL.Query().Get("ID")
		}
	} else if r.Method == http.MethodPost {
		requestBody, err := io.ReadAll(r.Body)
		if err != nil {
			log.Println("Failed to read the request body:", err)
			http.Error(w, "Failed to read the request body", http.StatusInternalServerError)
			return
		}

		var requestBodyJSON map[string]string
		err = json.Unmarshal(requestBody, &requestBodyJSON)
		if err != nil {
			log.Println("Failed to parse the request body:", err)
			http.Error(w, "Failed to parse the request body", http.StatusInternalServerError)
			return
		}

		if requestBodyJSON["subchannel"] != "" {
			subChannel = requestBodyJSON["subchannel"]
		}

		if requestBodyJSON["subChannel"] != "" {
			subChannel = requestBodyJSON["subChannel"]
		}

		if requestBodyJSON["ID"] != "" {
			tunnelId = requestBodyJSON["ID"]
		}

		if requestBodyJSON["id"] != "" {
			tunnelId = requestBodyJSON["id"]
		}
	}

	if subChannel == "" {
		subChannel = "main"
	}

	if tunnelId == "" {
		log.Println("The request must contain a valid 'id' parameter or field")
		http.Error(w, "The request must contain a valid 'id' parameter or field", http.StatusBadRequest)
		return
	}

	tunnelsMutex.Lock()
	tunnel, exists := tunnels[tunnelId]
	if !exists {
		tunnelsMutex.Unlock()
		log.Println("No tunnel with this id exists:", tunnelId)
		http.Error(w, "No tunnel with this id exists.", http.StatusNotFound)
		return
	}
	if tunnel.SubChannels[subChannel] != "" {
		w.Header().Set("Content-Type", "application/json")
		response, err := json.Marshal(map[string]string{"content": tunnel.SubChannels[subChannel]})
		if err != nil {
			log.Println("Failed to encode response:", err)
			http.Error(w, "Failed to encode response", http.StatusInternalServerError)
			return
		}
		w.Write(response)
	}
	tunnelsMutex.Unlock()
	log.Println("Retrieved content for tunnel:", tunnelId, "subChannel:", subChannel)
}

func streamTunnelContent(w http.ResponseWriter, r *http.Request) {
	tunnelId := ""
	subChannel := ""
	if r.Method == http.MethodGet {
		tunnelId = r.URL.Query().Get("id")
		subChannel = r.URL.Query().Get("subChannel")
		if r.URL.Query().Get("subchannel") != "" {
			subChannel = r.URL.Query().Get("subchannel")
		}
		if r.URL.Query().Get("ID") != "" {
			tunnelId = r.URL.Query().Get("ID")
		}
	} else if r.Method == http.MethodPost {
		requestBody, err := io.ReadAll(r.Body)
		if err != nil {
			log.Println("Failed to read the request body:", err)
			http.Error(w, "Failed to read the request body", http.StatusInternalServerError)
			return
		}

		var requestBodyJSON map[string]string
		err = json.Unmarshal(requestBody, &requestBodyJSON)
		if err != nil {
			log.Println("Failed to parse the request body:", err)
			http.Error(w, "Failed to parse the request body", http.StatusInternalServerError)
			return
		}

		if requestBodyJSON["subchannel"] != "" {
			subChannel = requestBodyJSON["subchannel"]
		}

		if requestBodyJSON["subChannel"] != "" {
			subChannel = requestBodyJSON["subChannel"]
		}

		if requestBodyJSON["ID"] != "" {
			tunnelId = requestBodyJSON["ID"]
		}

		if requestBodyJSON["id"] != "" {
			tunnelId = requestBodyJSON["id"]
		}
	}

	if subChannel == "" {
		subChannel = "main"
	}

	if tunnelId == "" {
		log.Println("The request must contain a valid 'id' parameter or field")
		http.Error(w, "The request must contain a valid 'id' parameter or field", http.StatusBadRequest)
		return
	}

	tunnelsMutex.Lock()
	_, exists := tunnels[tunnelId]
	if !exists {
		tunnelsMutex.Unlock()
		log.Println("No tunnel with this id exists:", tunnelId)
		http.Error(w, "No tunnel with this id exists.", http.StatusNotFound)
		return
	}
	tunnelsMutex.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	clientChan := make(chan string)
	clientsMutex.Lock()
	if clients[tunnelId] == nil {
		clients[tunnelId] = make(map[string][]chan string)
	}
	clients[tunnelId][subChannel] = append(clients[tunnelId][subChannel], clientChan)
	clientsMutex.Unlock()

	log.Println("Client connected to stream for tunnel:", tunnelId, "subChannel:", subChannel)

	for {
		select {
		case msg := <-clientChan:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
			clientsMutex.Lock()
			for i, client := range clients[tunnelId][subChannel] {
				if client == clientChan {
					clients[tunnelId][subChannel] = append(clients[tunnelId][subChannel][:i], clients[tunnelId][subChannel][i+1:]...)
					break
				}
			}
			clientsMutex.Unlock()
			log.Println("Client disconnected from stream for tunnel:", tunnelId, "subChannel:", subChannel)
			return
		}
	}
}

func sendToTunnel(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		requestBody, err := io.ReadAll(r.Body)
		if err != nil {
			log.Println("Failed to read the request body:", err)
			http.Error(w, "Failed to read the request body", http.StatusInternalServerError)
			return
		}

		var requestBodyJSON map[string]string
		err = json.Unmarshal(requestBody, &requestBodyJSON)
		if err != nil {
			log.Println("Failed to parse the request body:", err)
			http.Error(w, "Failed to parse the request body", http.StatusInternalServerError)
			return
		}

		if requestBodyJSON["subchannel"] != "" {
			requestBodyJSON["subChannel"] = requestBodyJSON["subchannel"]
		}

		if requestBodyJSON["subChannel"] == "" {
			requestBodyJSON["subChannel"] = "main"
		}

		if requestBodyJSON["ID"] != "" {
			requestBodyJSON["id"] = requestBodyJSON["ID"]
		}

		if requestBodyJSON["id"] == "" || requestBodyJSON["subChannel"] == "" || requestBodyJSON["content"] == "" {
			log.Println("The request body must contain a valid 'id', 'subChannel' and 'content' field")
			http.Error(w, "The request body must contain a valid 'id', 'subChannel' and 'content' field", http.StatusBadRequest)
			return
		}

		tunnelsMutex.Lock()
		tunnel, exists := tunnels[requestBodyJSON["id"]]
		if !exists {
			tunnelsMutex.Unlock()
			log.Println("No tunnel with this id exists:", requestBodyJSON["id"])
			http.Error(w, "No tunnel with this id exists.", http.StatusNotFound)
			return
		}
		tunnel.SubChannels[requestBodyJSON["subChannel"]] = requestBodyJSON["content"]
		tunnelsMutex.Unlock()

		clientsMutex.Lock()
		for _, client := range clients[requestBodyJSON["id"]][requestBodyJSON["subChannel"]] {
			client <- requestBodyJSON["content"]
		}
		clientsMutex.Unlock()

		w.WriteHeader(http.StatusOK)
		log.Println("Sent content to tunnel:", requestBodyJSON["id"], "subChannel:", requestBodyJSON["subChannel"])
	} else if r.Method == http.MethodGet {
		id := r.URL.Query().Get("id")
		subChannel := r.URL.Query().Get("subChannel")
		content := r.URL.Query().Get("content")

		if r.URL.Query().Get("subchannel") != "" {
			subChannel = r.URL.Query().Get("subchannel")
		}

		if r.URL.Query().Get("ID") != "" {
			id = r.URL.Query().Get("ID")
		}

		if id == "" || subChannel == "" || content == "" {
			log.Println("The request must contain a valid 'id', 'subChannel' and 'content' parameters")
			http.Error(w, "The request must contain a valid 'id', 'subChannel' and 'content' parameters", http.StatusBadRequest)
			return
		}

		tunnelsMutex.Lock()
		tunnel, exists := tunnels[id]
		if !exists {
			tunnelsMutex.Unlock()
			log.Println("No tunnel with this id exists:", id)
			http.Error(w, "No tunnel with this id exists.", http.StatusNotFound)
			return
		}
		tunnel.SubChannels[subChannel] = content
		tunnelsMutex.Unlock()

		clientsMutex.Lock()
		for _, client := range clients[id][subChannel] {
			client <- content
		}
		clientsMutex.Unlock()
		w.WriteHeader(http.StatusOK)
		log.Println("Sent content to tunnel:", id, "subChannel:", subChannel)
	} else {
		log.Println("Method not allowed. Only POST and GET requests are allowed.")
		http.Error(w, "Method not allowed. Only POST and GET requests are allowed.", http.StatusMethodNotAllowed)
		return
	}
}

func createTunnel(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		requestBody, err := io.ReadAll(r.Body)
		if err != nil {
			log.Println("Failed to read the request body:", err)
			http.Error(w, "Failed to read the request body", http.StatusInternalServerError)
			return
		}

		var requestBodyJSON map[string]string
		err = json.Unmarshal(requestBody, &requestBodyJSON)
		if err != nil {
			log.Println("Failed to parse the request body:", err)
			http.Error(w, "Failed to parse the request body", http.StatusInternalServerError)
			return
		}

		if requestBodyJSON["id"] == "" {
			log.Println("The request body must contain a valid 'id' field")
			http.Error(w, "The request body must contain a valid 'id' field", http.StatusBadRequest)
			return
		}

		tunnelsMutex.Lock()
		tunnels[requestBodyJSON["id"]] = &Tunnel{ID: requestBodyJSON["id"], Content: "", SubChannels: make(map[string]string)}
		tunnelsMutex.Unlock()

		response, err := json.Marshal(map[string]string{"id": requestBodyJSON["id"]})
		if err != nil {
			log.Println("Error creating the tunnel:", err)
			http.Error(w, "Error creating the tunnel", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(response)
		log.Println("Created tunnel with ID:", requestBodyJSON["id"])
	} else if r.Method == http.MethodGet {
		if r.URL.Query().Get("id") == "" {
			tunnelId := generateRandomID(6)
			tunnelsMutex.Lock()
			tunnels[tunnelId] = &Tunnel{ID: tunnelId, Content: "", SubChannels: make(map[string]string)}
			tunnelsMutex.Unlock()

			w.Header().Set("Content-Type", "application/json")
			response, err := json.Marshal(map[string]string{"id": tunnelId})
			if err != nil {
				log.Println("Failed to encode response:", err)
				http.Error(w, "Failed to encode response", http.StatusInternalServerError)
				return
			}
			w.Write(response)
			log.Println("Created tunnel with random ID:", tunnelId)
		} else {
			tunnelsMutex.Lock()
			tunnels[r.URL.Query().Get("id")] = &Tunnel{ID: r.URL.Query().Get("id"), Content: "", SubChannels: make(map[string]string)}
			tunnelsMutex.Unlock()
			response, err := json.Marshal(map[string]string{"id": r.URL.Query().Get("id")})
			if err != nil {
				log.Println("Error creating the tunnel:", err)
				http.Error(w, "Error creating the tunnel", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(response)
			log.Println("Created tunnel with ID:", r.URL.Query().Get("id"))
		}
	} else {
		log.Println("Method not allowed. Only POST and GET requests are allowed.")
		http.Error(w, "Method not allowed. Only POST and GET requests are allowed.", http.StatusMethodNotAllowed)
		return
	}
}

func generateRandomID(amount int) string {
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZ123456789!@#$%&*_-+=;:,.<>/?"
	b := make([]byte, amount)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}

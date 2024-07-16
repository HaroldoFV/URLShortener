package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/mux"
	httpSwagger "github.com/swaggo/http-swagger"
	"log"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	_ "shortURLgenerator/docs"
)

const (
	AllowedChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	SlugLength   = 7
	APIVersion   = "v1"
)

var redisClient *redis.Client
var ctx = context.Background()

type ShortenRequest struct {
	Destination string `json:"destination"`
	Slug        string `json:"slug,omitempty"`
}

type ShortenResponse struct {
	Slug        string `json:"slug"`
	Destination string `json:"destination"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

// @title Encurtador de URLs API
// @version 1.0
// @description Este é um serviço de encurtamento de URLs.
// @host localhost
// @BasePath /
func main() {
	hostname, _ := os.Hostname()
	log.Printf("Starting service on %s", hostname)

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379" // Default Redis address
	}

	redisClient = redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})

	r := mux.NewRouter()

	r.Use(corsMiddleware)

	// API versioned routes
	api := r.PathPrefix("/api/" + APIVersion).Subrouter()
	api.HandleFunc("/shortlink", shortenHandler).Methods("POST", "OPTIONS")

	r.HandleFunc("/s/{slug}", redirectHandler).Methods("GET")

	r.PathPrefix("/swagger/").Handler(httpSwagger.Handler(
		httpSwagger.URL("http://localhost/swagger/doc.json"),
	))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // Default port
	}

	log.Printf("Server starting on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// @Summary Encurtar URL
// @Description Cria uma URL curta a partir de uma URL longa
// @Accept  json
// @Produce  json
// @Param   url     body    ShortenRequest     true        "URL para encurtar"
// @Success 200 {object} ShortenResponse
// @Router /api/v1/shortlink [post]
func shortenHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("Received request to /api/%s/shortlink", APIVersion)
	var req ShortenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("Error decoding request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Destination == "" {
		http.Error(w, "Destination URL is required", http.StatusBadRequest)
		return
	}

	var slug string
	var err error

	if req.Slug != "" {
		if !isValidSlug(req.Slug) {
			http.Error(w, "Invalid slug format", http.StatusBadRequest)
			return
		}

		exists, err := slugExists(req.Slug)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if exists {
			http.Error(w, "Slug already in use", http.StatusConflict)
			return
		}

		slug = req.Slug
	} else {
		slug, err = generateUniqueSlug()
		if err != nil {
			log.Printf("Error generating unique slug: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	err = saveURL(slug, req.Destination)
	if err != nil {
		log.Printf("Error saving URL: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := ShortenResponse{Slug: slug, Destination: req.Destination}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
	log.Printf("Successfully shortened URL: %s to %s", req.Destination, slug)
}

// @Summary Redirecionar para URL longa
// @Description Redireciona para a URL longa correspondente à URL curta ou exibe informações para documentação
// @Produce html
// @Produce json
// @Param slug path string true "slug"
// @Param doc query boolean false "Set to true to get JSON documentation response" default(true)
// @Success 200 {object} map[string]string "JSON response for documentation"
// @Success 302 {string} string "Redirecionamento para URL longa"
// @Failure 404 {string} string "Shortlink not found"
// @Router /s/{slug} [get]
func redirectHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	slug := vars["slug"]
	log.Printf("Received request for slug: %s", slug)

	url, err := redisClient.Get(ctx, "url:"+slug).Result()
	if errors.Is(err, redis.Nil) {
		log.Printf("URL not found in Redis for slug: %s", slug)
		respondWithError(w, http.StatusNotFound, "Shortlink not found")
		return
	} else if err != nil {
		log.Printf("Error retrieving URL from Redis: %v", err)
		respondWithError(w, http.StatusInternalServerError, "Internal server error")
		return
	}

	if strings.Contains(strings.ToLower(r.UserAgent()), "swagger") || r.URL.Query().Get("doc") == "true" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"message": "This is an example response for Swagger documentation",
			"slug":    slug,
			"action":  "redirect",
			"url":     url,
		})
		return
	}

	redisClient.Incr(ctx, "stats:"+slug)

	log.Printf("Redirecting %s to %s", slug, url)
	http.Redirect(w, r, url, http.StatusMovedPermanently)
}

func respondWithError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(ErrorResponse{Error: message})
}

func isValidSlug(slug string) bool {
	match, _ := regexp.MatchString("^[a-zA-Z0-9]{3,10}$", slug)
	return match
}

func slugExists(slug string) (bool, error) {
	exists, err := redisClient.Exists(ctx, "url:"+slug).Result()
	return exists == 1, err
}

func saveURL(slug, destination string) error {
	pipe := redisClient.Pipeline()
	pipe.Set(ctx, "url:"+slug, destination, 30*24*time.Hour)
	pipe.Set(ctx, "created:"+slug, time.Now().Format(time.RFC3339), 30*24*time.Hour)
	_, err := pipe.Exec(ctx)
	return err
}

func generateUniqueSlug() (string, error) {
	timestamp := time.Now().Unix()
	prefix := base62Encode(timestamp)[:3]

	for i := 0; i < 10; i++ {
		slug := prefix + generateRandomSlug(4)
		exists, err := slugExists(slug)
		if err != nil {
			return "", err
		}
		if !exists {
			return slug, nil
		}
	}
	return "", fmt.Errorf("unable to generate unique slug after multiple attmpts")
}

func generateRandomSlug(length int) string {
	b := make([]byte, length)
	for i := range b {
		b[i] = AllowedChars[rand.Intn(len(AllowedChars))]
	}
	return string(b)
}

func base62Encode(num int64) string {
	if num == 0 {
		return string(AllowedChars[0])
	}

	var encoded []byte
	for num > 0 {
		encoded = append(encoded, AllowedChars[num%62])
		num /= 62
	}

	for i, j := 0, len(encoded)-1; i < j; i, j = i+1, j-1 {
		encoded[i], encoded[j] = encoded[j], encoded[i]
	}

	return string(encoded)
}

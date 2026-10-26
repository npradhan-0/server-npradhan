package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

type RequestLog struct {
	Method     string `json:"method"`
	SourceIP   string `json:"source_ip"`
	Path       string `json:"path"`
	StatusCode int    `json:"status_code"`
	Timestamp  string `json:"timestamp"`
}

func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func logToLoggly(entry RequestLog) {
	token := os.Getenv("LOGGLY_TOKEN")
	if token == "" {
		log.Println("ERROR: LOGGLY_TOKEN is not set")
		return
	}

	data, err := json.Marshal(entry)
	if err != nil {
		log.Printf("Loggly JSON error: %v", err)
		return
	}

	url := fmt.Sprintf(
		"https://logs-01.loggly.com/inputs/%s/tag/go-http-server/",
		token,
	)

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		log.Printf("Loggly request error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Loggly delivery error: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("Loggly returned HTTP %d", resp.StatusCode)
	}
}

func handler(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK

	if r.Method != http.MethodGet {
		status = http.StatusMethodNotAllowed
	} else if r.URL.Path != "/npradhan/status" {
		status = http.StatusNotFound
	}

	// Set response headers before writing the status.
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", "GET")
	}

	// Recording every request, including 404 and 405.
	logToLoggly(RequestLog{
		Method:     r.Method,
		SourceIP:   sourceIP(r),
		Path:       r.URL.Path,
		StatusCode: status,
		Timestamp:  time.Now().Format(time.RFC3339),
	})

	w.WriteHeader(status)

	switch status {
	case http.StatusOK:
		_ = json.NewEncoder(w).Encode(map[string]string{
			"time": time.Now().Format(time.RFC3339),
		})
	case http.StatusNotFound:
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "Not Found",
		})
	case http.StatusMethodNotAllowed:
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "Method Not Allowed",
		})
	}
}

func main() {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handler)

	log.Printf("Server listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

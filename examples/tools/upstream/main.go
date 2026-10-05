// Command upstream is a local stand-in for the services the example Spin
// applications call, so example tests never depend on a third-party
// endpoint.
//
//	GET  /                      plain-text target for examples/outbound-http
//	POST /v1/chat/completions   deterministic OpenAI-compatible completion
//	                            for examples/serverless-ai
//
// It is a test fixture, not an inference server.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	MaxCompletionTokens int `json:"max_completion_tokens"`
}

func main() {
	addr := flag.String("listen", "127.0.0.1:8090", "listen address")
	token := flag.String("require-token", "", "if set, /v1/chat/completions requires the header Authorization: bearer <token>")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprint(w, "outbound-ok\n")
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if *token != "" && !strings.EqualFold(r.Header.Get("Authorization"), "bearer "+*token) {
			http.Error(w, `{"error":{"message":"invalid token","type":"invalid_request_error"}}`, http.StatusUnauthorized)
			return
		}
		var req chatRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || len(req.Messages) == 0 {
			http.Error(w, `{"error":{"message":"invalid request","type":"invalid_request_error"}}`, http.StatusBadRequest)
			return
		}
		prompt := req.Messages[len(req.Messages)-1].Content
		text := fmt.Sprintf("mock completion from model %q for a %d-word prompt", req.Model, len(strings.Fields(prompt)))
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion",
			"created": 0,
			"model":   req.Model,
			"choices": []map[string]any{{
				"index":         0,
				"finish_reason": "stop",
				"message":       map[string]string{"role": "assistant", "content": text},
			}},
			"usage": map[string]int{
				"prompt_tokens":     len(strings.Fields(prompt)),
				"completion_tokens": len(strings.Fields(text)),
				"total_tokens":      len(strings.Fields(prompt)) + len(strings.Fields(text)),
			},
		})
	})

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("upstream listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}

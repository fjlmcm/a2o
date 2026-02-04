package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net"
	"crypto/tls"
	"os"
	"strings"
	"sort"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// --- Configuration Structs ---

type Config struct {
	DebugLevel        string          `json:"debug_level"` // "info" or "debug"
	RoundRobinAddress string          `json:"round_robin_address"`
	AuthToken         string          `json:"auth_token"` // If set, clients must provide this key
	TimeoutSeconds    int             `json:"timeout_seconds"` // Default 300
	Services          []ServiceConfig `json:"services"`
}

type ServiceConfig struct {
	Comment       string `json:"comment,omitempty"`
	ListenAddress string `json:"listen_address"`
	OpenAIBaseURL string `json:"openai_base_url"`
	OpenAIAPIKey  string `json:"openai_api_key"`
	ForceModel    string `json:"force_model,omitempty"`
	UpstreamProxy string `json:"upstream_proxy,omitempty"` // e.g. "socks5://user:pass@host:port"
}

var config Config
// var usageLogChan = make(chan UsageRecord, 5000) // Large buffer for bursts
var usageLogChan = make(chan UsageRecord, 5000)


type UsageRecord struct {
	Time       time.Time
	Service    string
	Model      string
	DurationMs int64
	Prompt     int
	Completion int
	Total      int
}


// --- Runtime State ---
var rrCounter atomic.Uint64

// --- Connection Pool ---
var (
	clientPool   = make(map[string]*http.Client)
	clientPoolMu sync.RWMutex
)

// --- Object Pools for reducing GC pressure ---
var (
	bufferPool = sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}
	// Pool for reusing map objects in sendEvent
	eventDataPool = sync.Pool{
		New: func() interface{} {
			return make(map[string]interface{}, 8)
		},
	}
)

// getOrCreateClient returns a cached HTTP client for the given service config
func getOrCreateClient(svc ServiceConfig) *http.Client {
	// Create a unique key for this service configuration
	key := svc.OpenAIBaseURL + "|" + svc.UpstreamProxy

	// Try read lock first (fast path)
	clientPoolMu.RLock()
	if client, ok := clientPool[key]; ok {
		clientPoolMu.RUnlock()
		return client
	}
	clientPoolMu.RUnlock()

	// Need to create new client (slow path)
	clientPoolMu.Lock()
	defer clientPoolMu.Unlock()

	// Double-check after acquiring write lock
	if client, ok := clientPool[key]; ok {
		return client
	}

	// Create optimized transport with connection pooling
	transport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		MaxConnsPerHost:       100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: time.Duration(config.TimeoutSeconds) * time.Second,
		ForceAttemptHTTP2:     false, // Keep HTTP/1.1 for streaming compatibility
		DisableCompression:    true,  // Prevent Gzip buffering for streaming
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
	}

	// Configure proxy if specified
	if svc.UpstreamProxy != "" {
		if proxyUrl, err := url.Parse(svc.UpstreamProxy); err == nil {
			transport.Proxy = http.ProxyURL(proxyUrl)
		} else {
			log.Printf("[WARN] Invalid proxy URL for %s: %v", svc.OpenAIBaseURL, err)
		}
	}

	client := &http.Client{
		Transport: transport,
		// No timeout here - managed per-request via context
	}

	clientPool[key] = client
	log.Printf("[POOL] Created new HTTP client for %s", key)
	return client
}

// --- Anthropic Structures ---
type AnthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []AnthropicMessage `json:"messages"`
	System        interface{}        `json:"system,omitempty"`
	MaxTokens     int                `json:"max_tokens"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice    interface{}        `json:"tool_choice,omitempty"`
	Metadata      *AnthropicMetadata `json:"metadata,omitempty"`
}
type AnthropicMetadata struct {
	UserId string `json:"user_id,omitempty"`
}
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}
type AnthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}
type AnthropicContent struct {
	Type   string                 `json:"type"`
	Text   string                 `json:"text,omitempty"`
	Source *AnthropicSource       `json:"source,omitempty"`
	Id     string                 `json:"id,omitempty"`
	Name   string                 `json:"name,omitempty"`
	Input  map[string]interface{} `json:"input,omitempty"`
}
type AnthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}
type AnthropicResponse struct {
	Id           string             `json:"id"`
	Type         string             `json:"type"`
	Role         string             `json:"role"`
	Content      []AnthropicContent `json:"content"`
	Model        string             `json:"model"`
	StopReason   *string            `json:"stop_reason"`
	StopSequence *string            `json:"stop_sequence"`
	Usage        AnthropicUsage     `json:"usage"`
}
type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// --- OpenAI Structures ---
type OpenAIRequest struct {
	Model             string          `json:"model"`
	Messages          []OpenAIMessage `json:"messages"`
	MaxTokens         int             `json:"max_tokens,omitempty"`
	Stop              []string        `json:"stop,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	StreamOptions     *StreamOptions  `json:"stream_options,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	Tools             []OpenAITool    `json:"tools,omitempty"`
	ToolChoice        interface{}     `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	User              string          `json:"user,omitempty"`
}
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type OpenAITool struct {
	Type     string              `json:"type"`
	Function OpenAIUtilsFunction `json:"function"`
}
type OpenAIUtilsFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}
type OpenAIMessage struct {
	Role             string           `json:"role"`
	Content          json.RawMessage  `json:"content,omitempty"`
	Refusal          string           `json:"refusal,omitempty"`           // OpenAI Refusal
	ReasoningContent string           `json:"reasoning_content,omitempty"` // DeepSeek Reasoning
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallId       string           `json:"tool_call_id,omitempty"`
}
type OpenAIToolCall struct {
	Index    int                `json:"index,omitempty"`
	Id       string             `json:"id"`
	Type     string             `json:"type"`
	Function OpenAIFunctionCall `json:"function"`
}
type OpenAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type OpenAIContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *OpenAIImageURL `json:"image_url,omitempty"`
}
type OpenAIImageURL struct {
	URL string `json:"url"`
}
type OpenAIResponse struct {
	Id      string         `json:"id"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}
type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
type OpenAIChoice struct {
	Message      OpenAIMessage `json:"message"`
	FinishReason *string       `json:"finish_reason,omitempty"`
}
type OpenAIStreamResponse struct {
	Id      string               `json:"id"`
	Choices []OpenAIStreamChoice `json:"choices"`
	Usage   *OpenAIUsage          `json:"usage,omitempty"`
}
type OpenAIStreamChoice struct {
	Delta        OpenAIMessage `json:"delta"`
	FinishReason *string       `json:"finish_reason"`
}

// --- Main ---

func main() {
	configFile := flag.String("config", "config.json", "Path to config file")
	flag.Parse()
	loadConfig(*configFile)



	fmt.Printf("A2O Proxy Config Loaded. DebugLevel: %s\n", config.DebugLevel)
	if config.DebugLevel == "debug" {
		log.Println("[DEBUG MODE ENABLED] All debug logs will be printed")
	}
	
	
	// Init Global Client - REMOVED for stability
	// httpClient = &http.Client{
	// 	Timeout: time.Duration(config.TimeoutSeconds) * time.Second,
	// }

	// Start Aggregator Worker
	go aggregatorWorker()

	if len(config.Services) == 0 {
		log.Fatal("No services defined in config.")
	}

	var wg sync.WaitGroup

	// 1. Start Individual Service Listeners
	for i, svc := range config.Services {
		wg.Add(1)
		go func(idx int, s ServiceConfig) {
			defer wg.Done()
			mux := http.NewServeMux()
			
			// Static Handler (Fixed Service)
			handler := makeHandler(func() ServiceConfig { return s }, s.ListenAddress)
			
			mux.HandleFunc("/v1/messages", handler)
			addCommonEndpoints(mux)

			log.Printf("Starting Service #%d on %s (%s)", idx+1, s.ListenAddress, s.Comment)
			if err := http.ListenAndServe(fixAddr(s.ListenAddress), mux); err != nil {
				log.Printf("[ERR] Service %s failed: %v", s.ListenAddress, err)
			}
		}(i, svc)
	}

	// 2. Start Global Round-Robin Listener (if configured)
	if config.RoundRobinAddress != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mux := http.NewServeMux()

			// Round Robin Provider
			rrProvider := func() ServiceConfig {
				count := rrCounter.Add(1)
				idx := (count - 1) % uint64(len(config.Services))
				selected := config.Services[idx]
				logDebug("[RR-LB] Selected Service #%d for request", idx+1)
				return selected
			}

			handler := makeHandler(rrProvider, config.RoundRobinAddress)
			mux.HandleFunc("/v1/messages", handler)
			addCommonEndpoints(mux)

			log.Printf("Starting Global Round-Robin Listener on %s", config.RoundRobinAddress)
			if err := http.ListenAndServe(fixAddr(config.RoundRobinAddress), mux); err != nil {
				log.Printf("[ERR] Round-Robin Listener %s failed: %v", config.RoundRobinAddress, err)
			}
		}()
	}

	wg.Wait()
}

func fixAddr(addr string) string {
	if !strings.Contains(addr, ":") {
		return ":" + addr
	}
	return addr
}

func addCommonEndpoints(mux *http.ServeMux) {
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200); w.Write([]byte("OK"))
	})
	mux.HandleFunc("/v1/messages/count_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		count := len(body) / 4
		if count < 1 { count = 1 }
		json.NewEncoder(w).Encode(map[string]int{"input_tokens": count})
	})
}

func loadConfig(path string) {
	// Default config
	config = Config{
		DebugLevel:     "info",
		TimeoutSeconds: 300,
		Services: []ServiceConfig{
			{
				ListenAddress: "127.0.0.1:8080",
				OpenAIBaseURL: "https://api.openai.com/v1/chat/completions",
			},
		},
	}
	file, err := os.Open(path)
	if err == nil {
		defer file.Close()
		json.NewDecoder(file).Decode(&config)
	} else if os.IsNotExist(err) {
		d, _ := json.MarshalIndent(config, "", "  ")
		os.WriteFile(path, d, 0644)
	}
	if config.DebugLevel == "" { config.DebugLevel = "info" }
	if config.TimeoutSeconds == 0 { config.TimeoutSeconds = 300 }
}

func logDebug(format string, v ...interface{}) {
	if config.DebugLevel == "debug" {
		log.Printf(format, v...)
	}
}

// --- Handler Factory & Logic ---

// configProvider is a function that returns the ServiceConfig to use for the current request.
// For static ports, it returns a fixed config. For RR ports, it returns a rotated config.
type configProvider func() ServiceConfig

func makeHandler(getServiceConfig configProvider, listenAddr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Always log incoming requests (not just in debug mode)
		log.Printf("[%s] >>> Request: %s %s from %s", listenAddr, r.Method, r.URL.Path, r.RemoteAddr)
		logDebug("[%s] Headers: %v", listenAddr, r.Header)

		// CORS
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		if r.Method == "OPTIONS" { w.WriteHeader(200); return }
		if r.Method != "POST" { http.Error(w, "Method Not Allowed", 405); return }

		// Auth Verification
		if config.AuthToken != "" {
			clientKey := r.Header.Get("x-api-key")
			if clientKey == "" {
				clientKey = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			}
			if clientKey != config.AuthToken {
				log.Printf("[AUTH] Failed auth attempt from %s using key: %s...", r.RemoteAddr, takePrefix(clientKey, 4))
				http.Error(w, "Unauthorized: Invalid API Key", 401)
				return
			}
			logDebug("[%s] Auth OK for %s", listenAddr, r.RemoteAddr)
		}

		svc := getServiceConfig() // Resolve config for this request
		svcName := svc.Comment
		if svcName == "" { svcName = svc.ListenAddress }

		logDebug("[%s] Using service: %s", listenAddr, svcName)

		// Decode Request
		var antReq AnthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&antReq); err != nil {
			logDebug("[%s] Bad request body: %v", listenAddr, err)
			http.Error(w, "Bad Request", 400); return
		}

		// Determining Target Model
		targetModel := antReq.Model // default
		if svc.ForceModel != "" {
			targetModel = svc.ForceModel
		}

		logDebug("[%s] REQ Model: %s -> %s, Stream: %v, MaxTokens: %d", listenAddr, antReq.Model, targetModel, antReq.Stream, antReq.MaxTokens)
		logDebug("[%s] Target URL: %s", listenAddr, svc.OpenAIBaseURL)

		// Convert
		oaiReq, err := convertToOpenAI(&antReq, targetModel)
		if err != nil {
			log.Printf("[ERR] Convert failed: %v", err)
			http.Error(w, "Convert Error", 400); return
		}

		// Forward - use pooled buffer
		buf := bufferPool.Get().(*bytes.Buffer)
		buf.Reset()
		json.NewEncoder(buf).Encode(oaiReq)
		oaiBody := buf.Bytes()
		defer bufferPool.Put(buf)

		// Auth Prepare
		authKey := svc.OpenAIAPIKey
		if authKey == "" {
			k := r.Header.Get("x-api-key")
			if k == "" { k = r.Header.Get("Authorization") }
			authKey = strings.TrimPrefix(k, "Bearer ")
		}

		// Use pooled HTTP client with connection reuse
		client := getOrCreateClient(svc)

		// Retry Logic
		var resp *http.Response
		var upstreamErr error
		var finalBody io.Reader // Keep track of the final body (wrapped or original)
		maxRetries := 3

		for i := 0; i < maxRetries; i++ {
			logDebug("[%s] Sending upstream request (attempt %d/%d)", listenAddr, i+1, maxRetries)

			// Re-create request for each attempt (Body is consumed)
			req, _ := http.NewRequestWithContext(r.Context(), "POST", svc.OpenAIBaseURL, bytes.NewBuffer(oaiBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+authKey) // Setup Auth

			resp, upstreamErr = client.Do(req)
			
			// 1. Network Error -> Retry
			if upstreamErr != nil {
				log.Printf("[WARN] Upstream attempt %d/%d failed: %v", i+1, maxRetries, upstreamErr)
				goto RETRY_WAIT
			}

			// 2. Non-200 Status -> Pass through immediately (Don't retry logic errors? Or maybe retry 5xx?)
			// Current logic: only retry network/stream errors. If API says 400/401/500, we show it.
			if resp.StatusCode != 200 {
				finalBody = resp.Body
				break // Exit loop, handle error below
			}

			// 3. Streaming Peek & Retry Logic
			if antReq.Stream {
				// We must read some data to ensure the stream isn't dead/empty.
				// This fixes the "200 followed by unexpected EOF" issue.
				
				// Create a buffer to hold peeked data
				var peekBuf bytes.Buffer
				peekReader := bufio.NewReader(resp.Body)
				success := false
				
				// Peek Timeout (5s)
				done := make(chan bool)
				go func() {
					// Read loop
					for {
						line, err := peekReader.ReadBytes('\n')
						if len(line) > 0 {
							peekBuf.Write(line)
						}
						
						if err != nil {
							// EOF or Error during peek
							break 
						}
						
						lineStr := string(line)
						// Check for valid SSE data
						if strings.HasPrefix(lineStr, "data:") {
							success = true
							break
						}
						// Continue if it's an empty line or comment (: ping)
					}
					done <- true
				}()
				
				select {
				case <-done:
					// Peek finished (either success or EOF/Network Error)
				case <-time.After(5 * time.Second):
					// Timeout
					log.Printf("[WARN] Stream Peek Timeout after 5s")
				}
				
				if success {
					// Reconstruct body: Peeked Data + Remaining Stream
					finalBody = io.MultiReader(&peekBuf, peekReader) // bufio.Reader can be read from directly for the rest
					break // Success!
				} else {
					// Peek failed (EOF without data, or Timeout)
					log.Printf("[WARN] Upstream Stream Dead/Empty on attempt %d/%d. Retrying...", i+1, maxRetries)
					resp.Body.Close()
					upstreamErr = fmt.Errorf("stream peek failed (empty or timeout)") // Set err to trigger retry
					// Fallthrough to RETRY_WAIT
				}
			} else {
				// Not streaming, just accept the response
				finalBody = resp.Body
				break
			}

		RETRY_WAIT:
			if i < maxRetries-1 {
				select {
				case <-r.Context().Done():
					http.Error(w, "Client Disconnected", 499)
					return
				case <-time.After(500 * time.Millisecond):
				}
			}
		}

		if upstreamErr != nil {
			log.Printf("[ERR] Upstream Call Failed after %d retries: %v", maxRetries, upstreamErr)
			http.Error(w, "Upstream Error", 502); return
		}
		// Note: resp is closed via finalBody wrapper or below if not used, 
		// but handleStream/handleNormal will consume it. 
		// Actually handleStream takes io.Reader. We need to close the underlying body eventually.
		// Since 'finalBody' wraps resp.Body, we rely on the handler to read it all.
		// BUT we must ensure resp.Body is closed. handleNormal does ReadAll. handleStream ends.
		// Let's add a defer helper or ensure Close is called.
		// A simple way: The original resp.Body is in finalBody.
		// We can defer resp.Body.Close() but that might close it while reading?
		// No, standard `defer resp.Body.Close()` at the end of this function is wrong because we handle async? 
		// No, handleStream is synchronous here.
		defer resp.Body.Close() 

		logDebug("[%s] Upstream response: %d %s", listenAddr, resp.StatusCode, resp.Status)

		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(finalBody) // Read from finalBody (which might be wrapped, or just resp.Body)
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			log.Printf("[ERR] Upstream replied %s: %s", resp.Status, string(body))
			return
		}

		if antReq.Stream {
			logDebug("[%s] Starting stream response handler", listenAddr)
			handleStream(w, finalBody, antReq.Model, svcName, start)
		} else {
			logDebug("[%s] Starting normal response handler", listenAddr)
			handleNormal(w, finalBody, antReq.Model, svcName, start)
		}
		

		logDebug("[%s] Finished in %v", listenAddr, time.Since(start))
	}
}

// Pass svcName down? Or closure? Closure is easier but makeHandler is generic.
// Let's change handleNormal/handleStream to return usage stats, then log here.
// But response body is read/streamed inside.
// Better approach: handleNormal invokes the log, but it needs stats.
// Actually, handleNormal/Stream calculate the stats. They should send to chan.
// Only missing piece is 'Service Name'. 
// We can pass service name to handle functions.

func convertToOpenAI(ant *AnthropicRequest, targetModel string) (*OpenAIRequest, error) {
	oai := &OpenAIRequest{
		Model:       targetModel,
		MaxTokens:   ant.MaxTokens,
		Stream:      ant.Stream,
		Temperature: ant.Temperature,
		TopP:        ant.TopP,
		Stop:        ant.StopSequences,
		Messages:    []OpenAIMessage{},
	}
	if ant.Stream {
		oai.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	// Tool Choice conversion
	// Anthropic: {"type": "auto"}, {"type": "any"}, {"type": "tool", "name": "xxx"}
	//            with optional "disable_parallel_tool_use": true
	// OpenAI: "auto", "required", {"type": "function", "function": {"name": "xxx"}}
	//         with "parallel_tool_calls": false
	if ant.ToolChoice != nil {
		switch tc := ant.ToolChoice.(type) {
		case string:
			// Simple string format (legacy)
			if tc == "auto" {
				oai.ToolChoice = "auto"
			} else if tc == "any" || tc == "required" {
				oai.ToolChoice = "required"
			} else if tc == "none" {
				oai.ToolChoice = "none"
			}
		case map[string]interface{}:
			tcType, _ := tc["type"].(string)
			if tcType == "auto" {
				oai.ToolChoice = "auto"
			} else if tcType == "any" {
				oai.ToolChoice = "required"
			} else if tcType == "tool" {
				// Specific tool
				toolName, _ := tc["name"].(string)
				if toolName != "" {
					oai.ToolChoice = map[string]interface{}{
						"type": "function",
						"function": map[string]string{
							"name": toolName,
						},
					}
				}
			}

			// Handle disable_parallel_tool_use
			if disableParallel, ok := tc["disable_parallel_tool_use"].(bool); ok && disableParallel {
				parallelFalse := false
				oai.ParallelToolCalls = &parallelFalse
			}
		}
	}

	// Metadata conversion: user_id -> user
	if ant.Metadata != nil && ant.Metadata.UserId != "" {
		oai.User = ant.Metadata.UserId
	}

	// Tools
	if len(ant.Tools) > 0 {
		oai.Tools = make([]OpenAITool, len(ant.Tools))
		for i, t := range ant.Tools {
			oai.Tools[i] = OpenAITool{
				Type: "function",
				Function: OpenAIUtilsFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.InputSchema,
				},
			}
		}
	}

	// System
	var systemPrompt string
	if ant.System != nil {
		if s, ok := ant.System.(string); ok {
			systemPrompt = s
		} else if arr, ok := ant.System.([]interface{}); ok {
			var sb strings.Builder
			for _, item := range arr {
				if m, ok := item.(map[string]interface{}); ok {
					if m["type"] == "text" {
						if txt, ok := m["text"].(string); ok {
							sb.WriteString(txt)
							sb.WriteByte('\n')
						}
					}
				}
			}
			systemPrompt = sb.String()
		}
	}
	if systemPrompt != "" {
		content, _ := json.Marshal(systemPrompt)
		oai.Messages = append(oai.Messages, OpenAIMessage{
			Role:    "system",
			Content: content,
		})
	}

	// Messages
	for _, msg := range ant.Messages {
		if msg.Role == "assistant" {
			var texts []string
			var thinkingTexts []string
			var toolCalls []OpenAIToolCall
			strContent, ok := msg.Content.(string)
			if ok {
				texts = append(texts, strContent)
			} else {
				b, _ := json.Marshal(msg.Content)
				var list []map[string]interface{}
				json.Unmarshal(b, &list)
				for _, block := range list {
					bType, _ := block["type"].(string)
					if bType == "text" {
						if t, ok := block["text"].(string); ok { texts = append(texts, t) }
					} else if bType == "thinking" {
						// Handle thinking blocks - extract the thinking content
						if t, ok := block["thinking"].(string); ok {
							thinkingTexts = append(thinkingTexts, t)
						}
					} else if bType == "tool_use" {
						tID, _ := block["id"].(string)
						tName, _ := block["name"].(string)
						tInput := block["input"]
						inputJson, _ := json.Marshal(tInput)
						toolCalls = append(toolCalls, OpenAIToolCall{
							Id:   tID,
							Type: "function",
							Function: OpenAIFunctionCall{Name: tName, Arguments: string(inputJson)},
						})
					}
				}
			}
			oaiMsg := OpenAIMessage{Role: "assistant"}

			// Set reasoning_content for backends that support it (e.g., DeepSeek)
			if len(thinkingTexts) > 0 {
				oaiMsg.ReasoningContent = strings.Join(thinkingTexts, "\n")
			}

			if len(texts) > 0 {
				fullText := strings.Join(texts, "\n")
				b, _ := json.Marshal(fullText)
				oaiMsg.Content = b
			}
			if len(toolCalls) > 0 { oaiMsg.ToolCalls = toolCalls }
			oai.Messages = append(oai.Messages, oaiMsg)
		} else if msg.Role == "user" {
			strContent, ok := msg.Content.(string)
			if ok {
				b, _ := json.Marshal(strContent)
				oai.Messages = append(oai.Messages, OpenAIMessage{Role: "user", Content: b})
			} else {
				b, _ := json.Marshal(msg.Content)
				var list []map[string]interface{}
				json.Unmarshal(b, &list)
				var userParts []OpenAIContentPart
				var toolResults []OpenAIMessage
				for _, block := range list {
					bType, _ := block["type"].(string)
					if bType == "tool_result" {
						tID, _ := block["tool_use_id"].(string)
						isError, _ := block["is_error"].(bool)

						// Collect text and image parts from tool result content
						var textParts []string
						var imageParts []OpenAIContentPart

						if cStr, ok := block["content"].(string); ok {
							textParts = append(textParts, cStr)
						} else if cList, ok := block["content"].([]interface{}); ok {
							for _, sub := range cList {
								if subMap, ok := sub.(map[string]interface{}); ok {
									subType, _ := subMap["type"].(string)
									if subType == "text" {
										if txt, ok := subMap["text"].(string); ok {
											textParts = append(textParts, txt)
										}
									} else if subType == "image" {
										// Handle image in tool result
										if src, ok := subMap["source"].(map[string]interface{}); ok {
											srcType, _ := src["type"].(string)
											var imgUrl string
											if srcType == "url" {
												imgUrl, _ = src["url"].(string)
											} else {
												mediaType, _ := src["media_type"].(string)
												data, _ := src["data"].(string)
												if mediaType != "" && data != "" {
													imgUrl = fmt.Sprintf("data:%s;base64,%s", mediaType, data)
												}
											}
											if imgUrl != "" {
												imageParts = append(imageParts, OpenAIContentPart{
													Type: "image_url", ImageURL: &OpenAIImageURL{URL: imgUrl},
												})
											}
										}
									}
								}
							}
						}

						// Build result text with error prefix if needed
						resultText := strings.Join(textParts, "\n")
						if isError {
							resultText = "[ERROR] " + resultText
						}

						// If there are images, use multimodal content format
						if len(imageParts) > 0 {
							var contentParts []OpenAIContentPart
							if resultText != "" {
								contentParts = append(contentParts, OpenAIContentPart{Type: "text", Text: resultText})
							}
							contentParts = append(contentParts, imageParts...)
							contentJson, _ := json.Marshal(contentParts)
							toolResults = append(toolResults, OpenAIMessage{
								Role: "tool", ToolCallId: tID, Content: contentJson,
							})
						} else {
							// Text-only content
							toolResults = append(toolResults, OpenAIMessage{
								Role: "tool", ToolCallId: tID, Content: json.RawMessage(fmt.Sprintf("%q", resultText)),
							})
						}
					} else if bType == "text" {
						if txt, ok := block["text"].(string); ok {
							userParts = append(userParts, OpenAIContentPart{Type: "text", Text: txt})
						}
					} else if bType == "image" {
						src, ok := block["source"].(map[string]interface{})
						if !ok {
							continue
						}
						srcType, _ := src["type"].(string)
						var imageUrl string
						if srcType == "url" {
							// URL type image
							imageUrl, _ = src["url"].(string)
						} else {
							// base64 type image (default)
							mediaType, _ := src["media_type"].(string)
							data, _ := src["data"].(string)
							if mediaType != "" && data != "" {
								imageUrl = fmt.Sprintf("data:%s;base64,%s", mediaType, data)
							}
						}
						if imageUrl != "" {
							userParts = append(userParts, OpenAIContentPart{Type: "image_url", ImageURL: &OpenAIImageURL{URL: imageUrl}})
						}
					}
				}
				if len(userParts) > 0 {
					b, _ := json.Marshal(userParts)
					oai.Messages = append(oai.Messages, OpenAIMessage{Role: "user", Content: b})
				}
				oai.Messages = append(oai.Messages, toolResults...)
			}
		}
	}
	return oai, nil
}

func handleNormal(w http.ResponseWriter, body io.Reader, model string, svcName string, startTime time.Time) {
	var oaiResp OpenAIResponse
	if err := json.NewDecoder(body).Decode(&oaiResp); err != nil {
		http.Error(w, "Upstream decode error", 500); return
	}

	// Logging
	usageLogChan <- UsageRecord{
		Time: startTime, Service: svcName, Model: model, 
		DurationMs: time.Since(startTime).Milliseconds(),
		Prompt: oaiResp.Usage.PromptTokens, Completion: oaiResp.Usage.CompletionTokens, Total: oaiResp.Usage.TotalTokens,
	}

	antResp := AnthropicResponse{
		Id: "msg_" + oaiResp.Id, 
		Type: "message",
		Role: "assistant",
		Model: model, 
		Content: []AnthropicContent{},
		Usage: AnthropicUsage{
			InputTokens: oaiResp.Usage.PromptTokens, 
			OutputTokens: oaiResp.Usage.CompletionTokens,
		},
	}

	
	if len(oaiResp.Choices) > 0 {
		choice := oaiResp.Choices[0]
		msg := choice.Message
		for _, tc := range msg.ToolCalls {
			var args map[string]interface{}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			if args == nil { args = make(map[string]interface{}) }
			antResp.Content = append(antResp.Content, AnthropicContent{
				Type: "tool_use", Id: tc.Id, Name: tc.Function.Name, Input: args,
			})
		}
		if len(msg.Content) > 0 {
			var s string
			json.Unmarshal(msg.Content, &s)
			if s != "" {
				antResp.Content = append(antResp.Content, AnthropicContent{Type: "text", Text: s})
			}
		}

		// Map finish_reason to stop_reason
		// OpenAI: "stop", "length", "tool_calls", "content_filter", "function_call"
		// Anthropic: "end_turn", "max_tokens", "stop_sequence", "tool_use", "content_filter"
		reason := "end_turn"
		if choice.FinishReason != nil {
			fr := *choice.FinishReason
			switch fr {
			case "length":
				reason = "max_tokens"
			case "tool_calls", "function_call":
				reason = "tool_use"
			case "content_filter":
				reason = "content_filter"
			case "stop":
				reason = "end_turn"
			default:
				reason = "end_turn"
			}
		} else if len(msg.ToolCalls) > 0 {
			reason = "tool_use"
		}
		antResp.StopReason = &reason
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(antResp)
}

func handleStream(w http.ResponseWriter, body io.Reader, model string, svcName string, startTime time.Time) {
	logDebug("[STR] Starting stream for model %s", model)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable Nginx buffering
	
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	// Increase buffer size to 2MB to handle large tool calls
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)
	msgId := fmt.Sprintf("msg_%d", time.Now().Unix())
	
	sendEvent(w, "message_start", map[string]interface{}{
		"message": map[string]interface{}{
			"id": msgId, "type": "message", "role": "assistant", "content": []string{},
			"model": model, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	flusher.Flush() // Flush start

	currentBlockIndex := -1
	currentBlockType := "" // "text", "thinking", "tool"
	var finalUsage *OpenAIUsage
	var finishReason string = "end_turn" // Default
	chunkCount := 0
	lastLogTime := time.Now()

	// Map OpenAI tool call index to Anthropic block index
	// This is needed because OpenAI streams multiple tool calls with their own indices
	toolIndexMap := make(map[int]int) // OpenAI tool index -> Anthropic block index

	for scanner.Scan() {
		line := scanner.Text()
		// Robust parsing: Trim space after "data:"
		if !strings.HasPrefix(line, "data:") { continue }
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))

		if data == "" { continue }
		if data == "[DONE]" {
			logDebug("[STR] Received [DONE] after %d chunks", chunkCount)
			break
		}

		chunkCount++
		// Log progress every 10 seconds in debug mode
		if time.Since(lastLogTime) > 10*time.Second {
			logDebug("[STR] Stream progress: %d chunks received", chunkCount)
			lastLogTime = time.Now()
		}

		var chunk OpenAIStreamResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			log.Printf("[WARN] Stream JSON parse error: %v (Line: %s)", err, data)
			continue
		}

		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta

			// 0. Update Finish Reason if present
			if chunk.Choices[0].FinishReason != nil {
				fr := *chunk.Choices[0].FinishReason
				switch fr {
				case "length":
					finishReason = "max_tokens"
				case "content_filter":
					finishReason = "content_filter"
				case "tool_calls", "function_call":
					finishReason = "tool_use"
				default:
					finishReason = "end_turn"
				}
			}

			// 1. Tool Calls - handle ALL tool calls in the delta, not just the first one
			if len(delta.ToolCalls) > 0 {
				logDebug("[STR] Tool calls in delta: %d", len(delta.ToolCalls))
				for _, tc := range delta.ToolCalls {
					toolIdx := tc.Index
					logDebug("[STR] Tool[%d]: id=%s name=%s args=%s", toolIdx, tc.Id, tc.Function.Name, tc.Function.Arguments)

					// Transition check: close non-tool block if switching to tool
					if currentBlockType != "" && currentBlockType != "tool" {
						sendEvent(w, "content_block_stop", map[string]interface{}{"index": currentBlockIndex})
						flusher.Flush()
						currentBlockType = "tool"
					}

					if tc.Id != "" {
						// Start new tool block
						log.Printf("[STR] >>> Starting tool block: id=%s name=%s", tc.Id, tc.Function.Name)
						// Check if we already have a block for this tool index
						if existingBlockIdx, exists := toolIndexMap[toolIdx]; exists {
							// This shouldn't happen normally, but handle it gracefully
							sendEvent(w, "content_block_stop", map[string]interface{}{"index": existingBlockIdx})
							flusher.Flush()
						}

						currentBlockIndex++
						currentBlockType = "tool"
						toolIndexMap[toolIdx] = currentBlockIndex

						sendEvent(w, "content_block_start", map[string]interface{}{
							"index": currentBlockIndex,
							"content_block": map[string]interface{}{
								"type": "tool_use", "id": tc.Id, "name": tc.Function.Name, "input": map[string]string{},
							},
						})
						flusher.Flush()
					}

					if tc.Function.Arguments != "" {
						// Find the correct block index for this tool
						blockIdx, exists := toolIndexMap[toolIdx]
						if !exists {
							// Fallback to current block index if mapping not found
							blockIdx = currentBlockIndex
						}

						sendEvent(w, "content_block_delta", map[string]interface{}{
							"index": blockIdx,
							"delta": map[string]interface{}{
								"type": "input_json_delta", "partial_json": tc.Function.Arguments,
							},
						})
						flusher.Flush()
					}
				}
				continue
			}

			// 2. DeepSeek Reasoning (Thinking)
			if delta.ReasoningContent != "" {
				if currentBlockType != "thinking" {
					if currentBlockType != "" {
						sendEvent(w, "content_block_stop", map[string]interface{}{"index": currentBlockIndex})
						flusher.Flush()
					}
					currentBlockIndex++
					currentBlockType = "thinking"
					sendEvent(w, "content_block_start", map[string]interface{}{
						"index": currentBlockIndex,
						"content_block": map[string]string{"type": "thinking", "thinking": ""},
					})
					flusher.Flush()
				}

				sendEvent(w, "content_block_delta", map[string]interface{}{
					"index": currentBlockIndex, 
					"delta": map[string]interface{}{
						"type": "thinking_delta", "thinking": delta.ReasoningContent,
					},
				})
				flusher.Flush()
				continue
			}

			// 3. Normal Content (or Refusal)
			var content string
			if len(delta.Content) > 0 { 
				json.Unmarshal(delta.Content, &content) 
			} else if delta.Refusal != "" {
				// Handle OpenAI Refusal as text
				content = fmt.Sprintf("\n[Refusal: %s]\n", delta.Refusal)
			}

			if content != "" {
				if currentBlockType != "text" {
					if currentBlockType != "" {
						sendEvent(w, "content_block_stop", map[string]interface{}{"index": currentBlockIndex})
						flusher.Flush()
					}
					currentBlockIndex++
					currentBlockType = "text"
					sendEvent(w, "content_block_start", map[string]interface{}{
						"index": currentBlockIndex, 
						"content_block": map[string]string{"type": "text", "text": ""},
					})
					flusher.Flush()
				}

				sendEvent(w, "content_block_delta", map[string]interface{}{
					"index": currentBlockIndex, "delta": map[string]interface{}{"type": "text_delta", "text": content},
				})
				flusher.Flush()
			}
		}
		if chunk.Usage != nil {
			finalUsage = chunk.Usage
		}
	}
	// Handle leftovers - close all open tool blocks first
	if currentBlockType == "tool" && len(toolIndexMap) > 0 {
		// Close all tool blocks that were opened
		for _, blockIdx := range toolIndexMap {
			sendEvent(w, "content_block_stop", map[string]interface{}{"index": blockIdx})
			flusher.Flush()
		}
	} else if currentBlockType != "" {
		// Close the current non-tool block
		sendEvent(w, "content_block_stop", map[string]interface{}{"index": currentBlockIndex})
		flusher.Flush()
	} else {
		// Empty response (never entered any block)
		sendEvent(w, "content_block_start", map[string]interface{}{
			"index": 0, "content_block": map[string]string{"type": "text", "text": ""},
		})
		sendEvent(w, "content_block_stop", map[string]interface{}{"index": 0})
		flusher.Flush()
	}

	usageData := map[string]int{"output_tokens": 0}
	if finalUsage != nil {
		usageData["output_tokens"] = finalUsage.CompletionTokens
		
		// Logging
		usageLogChan <- UsageRecord{
			Time: startTime, Service: svcName, Model: model,
			DurationMs: time.Since(startTime).Milliseconds(),
			Prompt: finalUsage.PromptTokens, Completion: finalUsage.CompletionTokens, Total: finalUsage.TotalTokens,
		}
	}

	// Check for stream errors before sending final events
	streamErr := scanner.Err()
	if streamErr != nil {
		log.Printf("[STR] Stream error for %s: %v", model, streamErr)
		// Still send message_delta and message_stop so client knows stream ended (with error)
		finishReason = "error"
	}

	sendEvent(w, "message_delta", map[string]interface{}{
		"delta": map[string]interface{}{"stop_reason": finishReason, "stop_sequence": nil},
		"usage": usageData,
	})
	flusher.Flush()

	// Always send message_stop to properly terminate the stream
	sendEvent(w, "message_stop", map[string]interface{}{})
	flusher.Flush()

	duration := time.Since(startTime)
	if streamErr != nil {
		log.Printf("[STR] Stream finished with error for %s after %d chunks in %v: %v", model, chunkCount, duration, streamErr)
	} else {
		logDebug("[STR] Stream finished successfully for %s: %d chunks in %v", model, chunkCount, duration)
	}
}

func sendEvent(w io.Writer, eventType string, data map[string]interface{}) {
	data["type"] = eventType

	// Use pooled buffer for JSON encoding
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()

	// Write event line
	buf.WriteString("event: ")
	buf.WriteString(eventType)
	buf.WriteByte('\n')

	// Write data line
	buf.WriteString("data: ")
	json.NewEncoder(buf).Encode(data) // Encode adds newline
	buf.WriteByte('\n')

	w.Write(buf.Bytes())
	bufferPool.Put(buf)
}

func takePrefix(s string, l int) string {
	if len(s) > l {
		return s[:l]
	}
	return s
}



// --- Aggregation Logic ---

type StatKey struct {
	Date    string
	Service string
	Model   string
}
type StatValue struct {
	Requests   int
	Prompt     int
	Completion int
	Total      int
}

var statsMap = make(map[StatKey]*StatValue)
var statsMua sync.Mutex

const StatsFile = "usage_stats.csv"

func aggregatorWorker() {
	loadStats()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	dirty := false

	for {
		select {
		case record := <-usageLogChan:
			statsMua.Lock()
			date := record.Time.Format("2006-01-02")
			key := StatKey{Date: date, Service: record.Service, Model: record.Model}
			val, exists := statsMap[key]
			if !exists {
				val = &StatValue{}
				statsMap[key] = val
			}
			val.Requests++
			val.Prompt += record.Prompt
			val.Completion += record.Completion
			val.Total += record.Total
			statsMua.Unlock()
			dirty = true
		
		case <-ticker.C:
			if dirty {
				saveStats()
				dirty = false
			}
		}
	}
}

func loadStats() {
	f, err := os.Open(StatsFile)
	if err != nil { return }
	defer f.Close()

	reader := csv.NewReader(f)
	// Skip header
	if _, err := reader.Read(); err != nil { return }

	statsMua.Lock()
	defer statsMua.Unlock()

	for {
		record, err := reader.Read()
		if err == io.EOF { break }
		if err != nil { continue }
		if len(record) < 7 { continue }

		// Format: Date,Service,Model,Requests,Prompt,Completion,Total
		date := record[0]
		svc := record[1]
		model := record[2]
		
		var reqs, p, c, t int
		fmt.Sscanf(record[3], "%d", &reqs)
		fmt.Sscanf(record[4], "%d", &p)
		fmt.Sscanf(record[5], "%d", &c)
		fmt.Sscanf(record[6], "%d", &t)
		
		statsMap[StatKey{date, svc, model}] = &StatValue{
			Requests: reqs, Prompt: p, Completion: c, Total: t,
		}
	}
}

func saveStats() {
	statsMua.Lock()
	defer statsMua.Unlock()

	// Atomic Write: Write to temp file first, then rename.
	// This prevents corruption if the program crashes during write.
	tempFile := StatsFile + ".tmp"
	f, err := os.Create(tempFile)
	if err != nil {
		log.Printf("[ERR] Failed to create temp stats file: %v", err)
		return
	}
	// Note: We close 'f' manually below to ensure flush before rename, defer is backup
	defer f.Close()

	writer := csv.NewWriter(f)
	writer.Write([]string{"Date", "Service", "Model", "Requests", "Prompt", "Completion", "Total"})

	// Sort keys
	var keys []StatKey
	for k := range statsMap {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Date != keys[j].Date { return keys[i].Date > keys[j].Date } // Descending Date
		if keys[i].Service != keys[j].Service { return keys[i].Service < keys[j].Service }
		return keys[i].Model < keys[j].Model
	})

	for _, k := range keys {
		v := statsMap[k]
		writer.Write([]string{
			k.Date, k.Service, k.Model,
			fmt.Sprintf("%d", v.Requests),
			fmt.Sprintf("%d", v.Prompt),
			fmt.Sprintf("%d", v.Completion),
			fmt.Sprintf("%d", v.Total),
		})
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		log.Printf("[ERR] CSV Write Error: %v", err)
		return
	}
	f.Close() // Close before rename

	// Atomic Rename
	if err := os.Rename(tempFile, StatsFile); err != nil {
		// If rename fails (e.g. windows locking), remove temp
		log.Printf("[ERR] Failed to rename stats file: %v", err)
		os.Remove(tempFile)
	}
}

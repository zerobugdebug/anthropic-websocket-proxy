package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/apigatewaymanagementapi"
)

const (
	defaultAnthropicModel   = "claude-3-5-sonnet-2024062"
	defaultAnthropicVersion = "2023-06-01"
	connectRouteKey         = "$connect"
	disconnectRouteKey      = "$disconnect"
	envAnthropicURL         = "ANTHROPIC_URL"
	envAnthropicKey         = "ANTHROPIC_KEY"
	envAnthropicModel       = "ANTHROPIC_MODEL"
	envAnthropicVersion     = "ANTHROPIC_VERSION"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	PromptTemplate string    `json:"prompt_template"`
	Messages       []Message `json:"messages"`
}

type AnthropicResponse struct {
	Type    string `json:"type"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// AnthropicMessage represents a single message in the conversation
type AnthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// AnthropicRequest represents the full request structure for the Anthropic API
type AnthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	Messages    []AnthropicMessage `json:"messages"`
	Stream      bool               `json:"stream,omitempty"`
	Temperature float64            `json:"temperature,omitempty"`
	System      string             `json:"system,omitempty"`
}

type Config struct {
	AnthropicURL     string
	AnthropicKey     string
	AnthropicModel   string
	AnthropicVersion string
}

// createResponse creates an API Gateway response with a specified message and status code
func createResponse(message string, statusCode int, headers map[string]string) (events.APIGatewayProxyResponse, error) {
	var retErr error
	if statusCode != http.StatusOK {
		retErr = fmt.Errorf(message, statusCode)
	}

	response := events.APIGatewayProxyResponse{
		Body:       message,
		StatusCode: statusCode,
	}

	if len(headers) > 0 {
		response.Headers = headers
	}

	return response, retErr
}

// loadConfig loads configuration from environment variables
func loadConfig() (Config, error) {
	cfg := Config{
		AnthropicURL:     os.Getenv(envAnthropicURL),
		AnthropicKey:     os.Getenv(envAnthropicKey),
		AnthropicModel:   os.Getenv(envAnthropicModel),
		AnthropicVersion: os.Getenv(envAnthropicVersion),
	}

	if cfg.AnthropicKey == "" {
		return cfg, fmt.Errorf("OpenAI API key not found in environment variable OPENAI_API_KEY")
	}

	if cfg.AnthropicModel == "" {
		cfg.AnthropicModel = defaultAnthropicModel
	}

	if cfg.AnthropicVersion == "" {
		cfg.AnthropicVersion = defaultAnthropicVersion
	}

	if cfg.AnthropicURL == "" {
		return cfg, fmt.Errorf("API Gateway Endpoint not found in environment variable API_GW_ENDPOINT")
	}

	return cfg, nil
}

func handleRequest(ctx context.Context, event events.APIGatewayWebsocketProxyRequest) (events.APIGatewayProxyResponse, error) {
	switch event.RequestContext.RouteKey {
	case connectRouteKey:
		return handleConnect(event)
	case disconnectRouteKey:
		return handleDisconnect(event)
	default:
		return handleSendMessage(ctx, event)
	}
}

func handleConnect(event events.APIGatewayWebsocketProxyRequest) (events.APIGatewayProxyResponse, error) {
	fmt.Printf("Client connected: %s", event.RequestContext.ConnectionID)
	return createResponse("Connected successfully", http.StatusOK, map[string]string{"Sec-WebSocket-Protocol": event.Headers["Sec-WebSocket-Protocol"]})
	//return createResponse("Connected successfully", http.StatusOK)
}

func handleDisconnect(event events.APIGatewayWebsocketProxyRequest) (events.APIGatewayProxyResponse, error) {
	fmt.Printf("Client disconnected: %s", event.RequestContext.ConnectionID)
	return createResponse("Disconnected successfully", http.StatusOK, map[string]string{"Sec-WebSocket-Protocol": event.Headers["Sec-WebSocket-Protocol"]})
}

func handleSendMessage(ctx context.Context, event events.APIGatewayWebsocketProxyRequest) (events.APIGatewayProxyResponse, error) {
	fmt.Printf("event.Resource: %v\n", event.Resource)
	fmt.Printf("event.Path: %v\n", event.Path)
	fmt.Printf("event.HTTPMethod: %v\n", event.HTTPMethod)
	fmt.Printf("event.Body: %v\n", event.Body)
	fmt.Printf("event.RequestContext: %v\n", event.RequestContext)
	fmt.Printf("event.RequestContext.RouteKey: %v\n", event.RequestContext.RouteKey)

	// Parse the incoming request
	var req Request
	err := json.Unmarshal([]byte(event.Body), &req)
	if err != nil {
		return createResponse(fmt.Sprintf("Error parsing request JSON: %s", err), http.StatusBadRequest, nil)
	}

	// Create a channel to receive text blocks
	textChan := make(chan string)
	errorChan := make(chan error, 1)
	doneChan := make(chan struct{})

	go func() {
		defer close(textChan)
		err := callAnthropicAPI(req, textChan, doneChan)
		if err != nil {
			errorChan <- err
		}
		close(errorChan)
	}()

	wsClient, err := createWebSocketClient(ctx, event.RequestContext.DomainName, event.RequestContext.Stage)
	if err != nil {
		return createResponse(fmt.Sprintf("Failed to create WebSocket client: %v", err), http.StatusInternalServerError, nil)
	}
	fmt.Printf("wsClient: %v\n", wsClient)

	for {
		select {
		case text, ok := <-textChan:
			fmt.Printf("text: %v\n", text)
			if !ok {
				return createResponse("Message processing completed", http.StatusOK, map[string]string{"Sec-WebSocket-Protocol": event.Headers["Sec-WebSocket-Protocol"]})
			}
			err = sendWebSocketMessage(ctx, wsClient, event.RequestContext.ConnectionID, text)
			if err != nil {
				return createResponse(fmt.Sprintf("Failed to send WebSocket message: %v", err), http.StatusInternalServerError, nil)
			}
		case err := <-errorChan:
			fmt.Printf("err: %v\n", err)
			if err != nil {
				return createResponse(fmt.Sprintf("Error calling Anthropic API: %v", err), http.StatusInternalServerError, nil)
			}
		case <-doneChan:
			// Close the WebSocket connection
			err = closeWebSocketConnection(ctx, wsClient, event.RequestContext.ConnectionID)
			if err != nil {
				return createResponse(fmt.Sprintf("Failed to close WebSocket connection: %v", err), http.StatusInternalServerError, nil)
			}
			return createResponse("Message processing completed", http.StatusOK, map[string]string{"Sec-WebSocket-Protocol": event.Headers["Sec-WebSocket-Protocol"]})
		case <-ctx.Done():
			return createResponse("Request timeout", http.StatusGatewayTimeout, nil)
		}
	}
}

// NewAnthropicRequest creates a new AnthropicRequest with default values
func NewAnthropicRequest(model string, system string, messages []AnthropicMessage) *AnthropicRequest {
	return &AnthropicRequest{
		Model:     model,
		MaxTokens: 1024,
		Messages:  messages,
		Stream:    true,
		System:    system,
	}
}

// MarshalRequest marshals the AnthropicRequest into JSON
func MarshalRequest(req *AnthropicRequest) ([]byte, error) {
	return json.Marshal(req)
}

// Function to convert received Request to AnthropicRequest
func ConvertToAnthropicRequest(req Request, model string, system string) *AnthropicRequest {
	messages := make([]AnthropicMessage, len(req.Messages))
	for i, msg := range req.Messages {
		messages[i] = AnthropicMessage(msg)
	}
	return NewAnthropicRequest(model, system, messages)
}

func callAnthropicAPI(req Request, textChan chan<- string, doneChan chan<- struct{}) error {

	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("error loading config: %w", err)
	}
	fmt.Printf("config: %v\n", config)

	anthropicURL := config.AnthropicURL
	anthropicAPIKey := config.AnthropicKey
	anthropicModel := config.AnthropicModel
	anthropicVersion := config.AnthropicVersion
	systemPrompt := os.Getenv(req.PromptTemplate)
	if systemPrompt == "" {
		fmt.Printf("system prompt [%s] was not found", req.PromptTemplate)
	}

	anthropicReq := ConvertToAnthropicRequest(req, anthropicModel, systemPrompt)

	requestBody, err := MarshalRequest(anthropicReq)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}
	fmt.Printf("requestBody: %v\n", requestBody)

	httpReq, err := http.NewRequest("POST", anthropicURL, bytes.NewReader(requestBody))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", anthropicAPIKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var currentEvent string

	for scanner.Scan() {
		line := scanner.Text()
		fmt.Printf("line: %v\n", line)
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			fmt.Printf("currentEvent: %v\n", currentEvent)
		} else if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			fmt.Printf("data: %v\n", data)
			var eventData map[string]interface{}
			err := json.Unmarshal([]byte(data), &eventData)
			if err != nil {
				return err
			}
			fmt.Printf("eventData: %v\n", eventData)

			switch currentEvent {
			case "message_start":
				fmt.Println("Message started")
			case "content_block_start":
				fmt.Println("Content block started")
			case "ping":
				fmt.Println("Received ping")
			case "content_block_delta":
				if delta, ok := eventData["delta"].(map[string]interface{}); ok {
					if textDelta, ok := delta["text"].(string); ok {
						textChan <- textDelta
						fmt.Println("[" + textDelta + "]")
					}
				}
			case "content_block_stop":
				fmt.Println("Content block stopped")
			case "message_delta":
				fmt.Println("Received message delta")
			case "message_stop":
				fmt.Println("Message stopped")
				close(doneChan) // Signal completion
				return nil
			default:
				fmt.Printf("Unhandled event type: %s", currentEvent)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}

func createWebSocketClient(ctx context.Context, domainName, stage string) (*apigatewaymanagementapi.Client, error) {
	cfg, err := awsConfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %v", err)
	}

	client := apigatewaymanagementapi.NewFromConfig(cfg, func(o *apigatewaymanagementapi.Options) {
		//		o.EndpointResolverV2 = apigatewaymanagementapi.EndpointResolverV2FromURL(fmt.Sprintf("https://%s/%s", domainName, stage))
		fmt.Printf("URL: https://%s/%s", domainName, stage)
		o.BaseEndpoint = aws.String(fmt.Sprintf("https://%s/%s", domainName, stage))
	})

	return client, nil
}

func closeWebSocketConnection(ctx context.Context, client *apigatewaymanagementapi.Client, connectionID string) error {
	_, err := client.DeleteConnection(ctx, &apigatewaymanagementapi.DeleteConnectionInput{
		ConnectionId: aws.String(connectionID),
	})
	return err
}

func sendWebSocketMessage(ctx context.Context, client *apigatewaymanagementapi.Client, connectionID string, message string) error {
	_, err := client.PostToConnection(ctx, &apigatewaymanagementapi.PostToConnectionInput{
		ConnectionId: aws.String(connectionID),
		Data:         []byte(message),
	})
	if err != nil {
		fmt.Printf("sendWebSocketMessage: Failed to send WebSocket message: %v", err)
	}
	return err
}

func main() {
	lambda.Start(handleRequest)
}

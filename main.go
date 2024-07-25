package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/apigatewaymanagementapi"
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

type Config struct {
	AnthropicKey       string
	AnthropicModel     string
	APIGatewayEndpoint string
}

var config Config // Global configuration variable

// errorResponse creates an error response with a specified message and status code
func errorResponse(message string, statusCode int) (events.APIGatewayProxyResponse, error) {
	return events.APIGatewayProxyResponse{
		Body:       message,
		StatusCode: statusCode,
	}, nil
}

// getAPIGatewayClient initializes and returns an API Gateway client
func getAPIGatewayClient() *apigatewaymanagementapi.ApiGatewayManagementApi {
	apiEndpoint := config.APIGatewayEndpoint
	return apigatewaymanagementapi.New(session.Must(session.NewSession()), aws.NewConfig().WithEndpoint(apiEndpoint))
}

func handleRequest(ctx context.Context, event events.APIGatewayWebsocketProxyRequest) (events.APIGatewayProxyResponse, error) {
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
		return events.APIGatewayProxyResponse{StatusCode: http.StatusBadRequest}, err
	}

	// Create a channel to receive text blocks
	textChan := make(chan string)

	// Start a goroutine to call Anthropic API and send text blocks to the channel
	go func() {
		defer close(textChan)
		err := callAnthropicAPI(req, textChan)
		if err != nil {
			// In a real-world scenario, you'd want to handle this error more gracefully
			fmt.Println("Error calling Anthropic API:", err)
		}
	}()

	// Create a WebSocket client
	wsClient, err := createWebSocketClient(ctx, event.RequestContext.ConnectionID, event.RequestContext.DomainName, event.RequestContext.Stage)
	if err != nil {
		return events.APIGatewayProxyResponse{StatusCode: 500}, err
	}

	// Stream text blocks to the WebSocket client
	for text := range textChan {
		err = sendWebSocketMessage(ctx, wsClient, text)
		if err != nil {
			return events.APIGatewayProxyResponse{StatusCode: 500}, err
		}
	}

	return events.APIGatewayProxyResponse{StatusCode: 200}, nil
}

func callAnthropicAPI(req Request, textChan chan<- string) error {
	// Implement the logic to call Anthropic API and process the stream
	// This is a placeholder implementation
	anthropicURL := "https://api.anthropic.com/v1/messages"
	anthropicAPIKey := "YOUR_ANTHROPIC_API_KEY"

	// Marshal the messages
	messagesJSON, err := json.Marshal(req.Messages)
	if err != nil {
		return fmt.Errorf("failed to marshal messages: %w", err)
	}

	// Construct the request body
	requestBody := fmt.Sprintf(`{"model": "claude-3-haiku-20240307", "max_tokens": 1024, "messages": %s}`, string(messagesJSON))

	httpReq, err := http.NewRequest("POST", anthropicURL, strings.NewReader(requestBody))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", anthropicAPIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

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
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var eventData map[string]interface{}
			err := json.Unmarshal([]byte(data), &eventData)
			if err != nil {
				return err
			}

			switch currentEvent {
			case "content_block_delta":
				if delta, ok := eventData["delta"].(map[string]interface{}); ok {
					if textDelta, ok := delta["text"].(string); ok {
						textChan <- textDelta
					}
				}
			case "message_stop":
				// End of message, we can return
				return nil
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}

func createWebSocketClient(ctx context.Context, connectionID, domainName, stage string) (*apigatewaymanagementapi.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}

	client := apigatewaymanagementapi.NewFromConfig(cfg, func(o *apigatewaymanagementapi.Options) {
		o.EndpointResolver = apigatewaymanagementapi.EndpointResolverFromURL(fmt.Sprintf("https://%s/%s", domainName, stage))
	})

	return client, nil
}

func sendWebSocketMessage(ctx context.Context, client *apigatewaymanagementapi.Client, message string) error {
	_, err := client.PostToConnection(ctx, &apigatewaymanagementapi.PostToConnectionInput{
		ConnectionId: aws.String(connectionID),
		Data:         []byte(message),
	})
	return err
}

func main() {
	lambda.Start(handleRequest)
}

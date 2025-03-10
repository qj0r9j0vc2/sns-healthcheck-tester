package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

type Message struct {
	Timestamp int64 `json:"timestamp"`
}

var (
	snsTopicARN         = os.Getenv("SNS_TOPIC_ARN")
	slackWebhookURL     = os.Getenv("SLACK_WEBHOOK_URL")
	pagerDutyRoutingKey = os.Getenv("PAGERDUTY_ROUTING_KEY")
	responseThreshold   = 10 * time.Second
	snsClient           *sns.Client
)

func publishTimestamp(ctx context.Context) error {
	currentTime := time.Now().UnixMilli()
	msg := Message{
		Timestamp: currentTime,
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	input := &sns.PublishInput{
		Message:  aws.String(string(payload)),
		TopicArn: aws.String(snsTopicARN),
	}

	_, err = snsClient.Publish(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to publish message: %w", err)
	}

	log.Printf("Published message with timestamp: %d", currentTime)
	return nil
}

func callbackHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "only POST is allowed", http.StatusMethodNotAllowed)
		return
	}

	var msg Message
	err := json.NewDecoder(r.Body).Decode(&msg)
	if err != nil {
		http.Error(w, "invalid message format", http.StatusBadRequest)
		return
	}

	receivedTime := time.Now()
	publishedTime := time.UnixMilli(msg.Timestamp)
	latency := receivedTime.Sub(publishedTime)

	log.Printf("Received callback. Published at: %s, Received at: %s, Latency: %s",
		publishedTime.Format(time.RFC3339), receivedTime.Format(time.RFC3339), latency)

	if latency > responseThreshold {
		err1 := sendAlert(fmt.Sprintf("High latency detected: %s", latency))
		if err1 != nil {
			log.Printf("Error sending alert: %v", err1)
		}
	} else {
		log.Println("Latency within acceptable range.")
	}

	w.WriteHeader(http.StatusOK)
}

func sendAlert(message string) error {
	if slackWebhookURL != "" {
		err := postJSON(slackWebhookURL, map[string]string{
			"text": message,
		})
		if err != nil {
			log.Printf("Failed to send Slack alert: %v", err)
		}
	}

	if pagerDutyRoutingKey != "" {
		pagerPayload := map[string]interface{}{
			"routing_key":  pagerDutyRoutingKey,
			"event_action": "trigger",
			"payload": map[string]interface{}{
				"summary":   message,
				"source":    "SNS Checker",
				"severity":  "error",
				"timestamp": time.Now().Format(time.RFC3339),
			},
		}
		err := postJSON("https://events.pagerduty.com/v2/enqueue", pagerPayload)
		if err != nil {
			log.Printf("Failed to send PagerDuty alert: %v", err)
			return err
		}
	}
	return nil
}

func postJSON(url string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(data))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP request returned status: %d", resp.StatusCode)
	}

	return nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Load AWS Config
	cfg, err := config.LoadDefaultConfig(ctx, config.WithCredentialsProvider(
		credentials.NewStaticCredentialsProvider(
			os.Getenv("AWS_ACCESS_KEY_ID"),
			os.Getenv("AWS_SECRET_ACCESS_KEY"),
			"",
		)))
	if err != nil {
		log.Fatalf("Unable to load AWS SDK config: %v", err)
	}

	// Initialize SNS Client
	snsClient = sns.NewFromConfig(cfg)

	// Set up HTTP server
	server := &http.Server{Addr: ":8080", Handler: http.DefaultServeMux}
	http.HandleFunc("/callback", callbackHandler)

	// Handle graceful shutdown
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		<-stop
		log.Println("Shutting down server...")
		cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Fatalf("HTTP server Shutdown failed: %v", err)
		}
	}()

	// Start HTTP server
	go func() {
		log.Println("Starting HTTP server on port 8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// Read interval from env
	intervalSeconds := 30
	if intervalStr := os.Getenv("PUBLISH_INTERVAL_SECONDS"); intervalStr != "" {
		if val, err := strconv.Atoi(intervalStr); err == nil {
			intervalSeconds = val
		}
	}
	ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
	defer ticker.Stop()

	// Periodically publish timestamps
	for {
		select {
		case <-ticker.C:
			err := publishTimestamp(ctx)
			if err != nil {
				log.Printf("Error publishing timestamp: %v", err)
				sendAlert("Error publishing timestamp: " + err.Error())
			}
		case <-ctx.Done():
			log.Println("Stopping timestamp publishing...")
			return
		}
	}
}

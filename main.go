package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
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
		Message:  awsString(string(payload)),
		TopicArn: awsString(snsTopicARN),
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
		err1 := sendAlert("High latency detected: " + latency.String())
		if err1 != nil {
			log.Printf("Error sending alert: %v", err1)
		}
	} else {
		log.Println("Latency within acceptable range.")
	}

	w.WriteHeader(http.StatusOK)
}

func sendAlert(message string) error {
	err := postJSON(slackWebhookURL, map[string]string{
		"text": message,
	})
	if err != nil {
		log.Printf("Failed to send Slack alert: %v", err)
	}

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
	// Corrected PagerDuty Events API endpoint URL
	err = postJSON("https://events.pagerduty.com/v2/enqueue", pagerPayload)
	if err != nil {
		log.Printf("Failed to send PagerDuty alert: %v", err)
		return err
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

func awsString(s string) *string {
	return &s
}

func main() {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("unable to load SDK config, %v", err)
	}
	snsClient = sns.NewFromConfig(cfg)

	http.HandleFunc("/callback", callbackHandler)
	go func() {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		log.Printf("Starting HTTP server on port %s", port)
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			log.Fatalf("failed to start server: %v", err)
		}
	}()

	intervalSeconds := 30
	if intervalStr := os.Getenv("PUBLISH_INTERVAL_SECONDS"); intervalStr != "" {
		if val, err := strconv.Atoi(intervalStr); err == nil {
			intervalSeconds = val
		}
	}
	ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
	defer ticker.Stop()

	for {
		err = publishTimestamp(ctx)
		if err != nil {
			log.Printf("Error publishing timestamp: %v", err)
			sendAlert("Error publishing timestamp: " + err.Error())
		}
		<-ticker.C
	}
}

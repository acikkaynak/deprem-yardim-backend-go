package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Shopify/sarama"
	"github.com/acikkaynak/backend-api-go/broker"
	"github.com/acikkaynak/backend-api-go/feeds"
	"github.com/acikkaynak/backend-api-go/pkg/logger"
	"github.com/acikkaynak/backend-api-go/repository"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

const (
	consumerGroupName             = "feeds_location_consumer"
	intentConsumerGroupName       = "feeds_intent_consumer"
	addressResolvedTopicName      = "topic.feeds.location"
	intentResolvedTopicName       = "topic.feeds.intent"
	AWS_TASK_METADATA_URL_ENV_VAR = "ECS_CONTAINER_METADATA_URI_V4"
)

var (
	clientCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "go_consumer_metrics",
	}, []string{"topic", "timestamp", "task_id"})

	taskID string
)

// Message will be handled in ConsumeClaim method.
func main() {
	http.HandleFunc("/healthcheck", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(200)
	})

	http.Handle("/metrics", promhttp.Handler())

	go func() {
		if err := http.ListenAndServe(":80", nil); err != nil {
			fmt.Fprintf(os.Stderr, "server could not started or stopped: %s", err)
		}
	}()

	client, err := broker.NewConsumerGroup(consumerGroupName)
	if err != nil {
		log.Logger().Panic(err.Error())
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	consumer := NewConsumer()
	go func() {
		for {
			if err := client.Consume(ctx, []string{intentResolvedTopicName, addressResolvedTopicName}, consumer); err != nil {
				log.Logger().Panic("Error from consumer:", zap.Error(err))
			}
			// check if context was cancelled, signaling that the addressResolvedConsumer should stop
			if ctx.Err() != nil {
				return
			}
			consumer.ready = make(chan bool)
		}
	}()
	<-consumer.ready
	log.Logger().Info("Sarama consumer up and running!...")

	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)
	healthy := true
	for healthy {
		select {
		case <-ctx.Done():
			log.Logger().Info("terminating: context cancelled")
			healthy = false
		case <-sigterm:
			log.Logger().Info("terminating: via signal")
			healthy = false
		}
	}

	cancel()
	if err = client.Close(); err != nil {
		log.Logger().Panic("Error closing client:", zap.Error(err))
	}
}

func (consumer *Consumer) intentResolveHandle(message *sarama.ConsumerMessage, session sarama.ConsumerGroupSession) {
	var messagePayload IntentMessagePayload
	if err := json.Unmarshal(message.Value, &messagePayload); err != nil {
		log.Logger().Error("deserialization IntentMessagePayload error", zap.String("message", string(message.Value)), zap.Error(err))
		session.MarkMessage(message, "")
		session.Commit()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	intents, err := sendIntentResolveRequest(messagePayload.FullText, messagePayload.FeedID)
	if err != nil {
		if err.Error() == "alakasiz veri" {
			if err := consumer.repo.DeleteFeedLocation(ctx, messagePayload.FeedID); err != nil {
				log.Logger().Error("", zap.Error(err))
			}
		}
		log.Logger().Error("", zap.Error(err))
		session.MarkMessage(message, "")
		session.Commit()
		return
	}

	needs, err := sendNeedsResolveRequest(messagePayload.FullText, messagePayload.FeedID)
	if err != nil {
		session.MarkMessage(message, "")
		session.Commit()
		return
	}

	if err := consumer.repo.UpdateLocationIntentAndNeeds(ctx, messagePayload.FeedID, intents, needs); err != nil {
		fmt.Fprintf(os.Stderr,
			"error updating feed entry, location intent and needs %#v error %s rawMessage %s",
			messagePayload, err.Error(), string(message.Value))
		return
	}

	session.MarkMessage(message, "")
	session.Commit()
}

func sendIntentResolveRequest(fullText string, feedID int64) (string, error) {
	jsonBytes, err := json.Marshal(IntentRequest{
		Inputs: fullText,
	})
	fmt.Println(os.Getenv("INTENT_RESOLVER_API_URL"))

	req, err := http.NewRequest("POST", os.Getenv("INTENT_RESOLVER_API_URL"), bytes.NewReader(jsonBytes))
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not prepare http request IntentMessagePayload error message %s error %s", fullText, err.Error())
		return "", err
	}
	req.Header.Add("Authorization", "Bearer "+os.Getenv("INTENT_RESOLVER_API_KEY"))
	req.Header.Add("content-type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "could not get response IntentMessagePayload feedID %d status %d", feedID, resp.StatusCode)
		return "", err
	}

	intentResp := &IntentResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&intentResp.Results); err != nil {
		fmt.Fprintf(os.Stderr, "could not get decode response IntentMessagePayload feedID %d err %s", feedID, err.Error())
		return "", err
	}

	if len(intentResp.Results) == 0 {
		fmt.Fprintf(os.Stderr, "no data found on response IntentMessagePayload feedID %d", feedID)
		return "", nil
	}

	intents := make([]string, 0)
	for _, val := range intentResp.Results[0] {
		if val.Score >= 0.4 {
			if val.Label == "Alakasiz" && val.Score >= 0.7 {
				return "", fmt.Errorf("alakasiz veri")
			}
			intents = append(intents, strings.ToLower(val.Label))
		}
	}

	return strings.Join(intents, ","), nil
}

func sendNeedsResolveRequest(fullText string, feedID int64) ([]feeds.NeedItem, error) {
	jsonBytes, err := json.Marshal(NeedsRequest{
		Inputs: []string{fullText},
	})

	req, err := http.NewRequest("POST", os.Getenv("NEEDS_RESOLVER_API_URL"), bytes.NewReader(jsonBytes))
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not prepare http request NeedsMessagePayload error message %s error %s", fullText, err.Error())
		return nil, err
	}

	req.Header.Add("Authorization", "Bearer "+os.Getenv("NEEDS_RESOLVER_API_KEY"))
	req.Header.Add("content-type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "could not get response NeedsMessagePayload feedID %d status %d", feedID, resp.StatusCode)
		return nil, err
	}

	needsResp := &NeedsResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&needsResp); err != nil {
		fmt.Fprintf(os.Stderr, "could not get decode response NeedsMessagePayload feedID %d err %s", feedID, err.Error())
		return nil, err
	}

	needs := make([]feeds.NeedItem, 0)
	if len(needsResp.Response) == 0 {
		fmt.Fprintf(os.Stderr, "no data found on response NeedsMessagePayload feedID %d", feedID)
		// ret empty
		return needs, nil
	}

	for _, tag := range needsResp.Response[0].Processed.DetailedIntentTags {
		needs = append(needs, feeds.NeedItem{
			Label:  strings.ToLower(tag),
			Status: true,
		})
	}

	return needs, nil
}

func (consumer *Consumer) addressResolveHandle(message *sarama.ConsumerMessage, session sarama.ConsumerGroupSession) {
	var messagePayload ConsumeMessagePayload
	if err := json.Unmarshal(message.Value, &messagePayload); err != nil {
		fmt.Fprintf(os.Stderr, "deserialization error message %s error %s", string(message.Value), err.Error())
		session.MarkMessage(message, "")
		session.Commit()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	messagePayload.Feed.Timestamp = time.Now()

	f := feeds.Feed{
		ID:               messagePayload.Feed.ID,
		FullText:         messagePayload.Feed.FullText,
		IsResolved:       messagePayload.Feed.IsResolved,
		Channel:          messagePayload.Feed.Channel,
		Timestamp:        messagePayload.Feed.Timestamp,
		Epoch:            messagePayload.Feed.Epoch,
		ExtraParameters:  messagePayload.Feed.ExtraParameters,
		FormattedAddress: messagePayload.Feed.FormattedAddress,
		Reason:           messagePayload.Feed.Reason,
	}

	err, entryID := consumer.repo.CreateFeed(ctx, f, messagePayload.Location)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error inserting feed entry and location message %#v error %s rawMessage %s", messagePayload, err.Error(), string(message.Value))
		return
	}

	intentPayloadByte, err := json.Marshal(IntentMessagePayload{
		FeedID:   entryID,
		FullText: messagePayload.Feed.FullText,
	})

	_, _, err = consumer.producer.SendMessage(&sarama.ProducerMessage{
		Topic: intentResolvedTopicName,
		Key:   sarama.StringEncoder(fmt.Sprintf("%d", entryID)),
		Value: sarama.ByteEncoder(intentPayloadByte),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error producing intent feedID %d error %s", messagePayload.Feed.ID, err.Error())
		session.MarkMessage(message, "")
		session.Commit()
		return
	}

	session.MarkMessage(message, "")
	session.Commit()
}

// Consumer represents a Sarama consumer group consumer
type Consumer struct {
	ready    chan bool
	repo     *repository.Repository
	producer sarama.SyncProducer
}

func NewConsumer() *Consumer {
	producer, err := broker.NewProducer()
	if err != nil {
		log.Logger().Panic("failed to init kafka producer. err:", zap.Error(err))
	}
	return &Consumer{
		ready:    make(chan bool),
		repo:     repository.New(),
		producer: producer,
	}
}

// Setup is run at the beginning of a new session, before ConsumeClaim
func (consumer *Consumer) Setup(sarama.ConsumerGroupSession) error {
	// Mark the consumer as ready
	close(consumer.ready)
	return nil
}

// Cleanup is run at the end of a session, once all ConsumeClaim goroutines have exited
func (consumer *Consumer) Cleanup(sarama.ConsumerGroupSession) error {
	return nil
}

// ConsumeClaim must start a consumer loop of ConsumerGroupClaim's Messages().
func (consumer *Consumer) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for {
		select {
		case message := <-claim.Messages():
			clientCounter.With(prometheus.Labels{
				"topic":     message.Topic,
				"timestamp": fmt.Sprintf("%d", message.Timestamp.Unix()),
				"task_id":   taskID,
			}).Inc()
			if message.Topic == intentResolvedTopicName {
				consumer.intentResolveHandle(message, session)
			}
			if message.Topic == addressResolvedTopicName {
				consumer.addressResolveHandle(message, session)
			}
		case <-session.Context().Done():
			return nil
		}
	}
}

type FeedMessage struct {
	ID               int64     `json:"id,omitempty"`
	FullText         string    `json:"raw_text"`
	IsResolved       bool      `json:"is_resolved"`
	Channel          string    `json:"channel,omitempty"`
	Timestamp        time.Time `json:"timestamp,omitempty"`
	Epoch            int64     `json:"epoch"`
	ExtraParameters  *string   `json:"extra_parameters,omitempty"`
	FormattedAddress string    `json:"formatted_address,omitempty"`
	Reason           *string   `json:"reason,omitempty"`
}

type ConsumeMessagePayload struct {
	Location feeds.Location `json:"location"`
	Feed     FeedMessage    `json:"feed"`
}

type IntentMessagePayload struct {
	FeedID   int64  `json:"id"`
	FullText string `json:"full_text"`
}

type IntentRequest struct {
	Inputs string `json:"inputs"`
}

type IntentResponse struct {
	Results []Intent
}

type Intent []struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
}

type NeedsRequest struct {
	Inputs []string `json:"inputs"`
}

type NeedsResponse struct {
	Response []struct {
		String    []string `json:"string"`
		Processed struct {
			Intent             []string `json:"intent"`
			DetailedIntentTags []string `json:"detailed_intent_tags"`
		} `json:"processed"`
	} `json:"response"`
}

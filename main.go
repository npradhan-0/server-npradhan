package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/gorilla/mux"
)

const tableName = "npradhan_StockQuotes"

type RequestLog struct {
	Method     string `json:"method"`
	SourceIP   string `json:"source_ip"`
	Path       string `json:"path"`
	StatusCode int    `json:"status_code"`
	Timestamp  string `json:"timestamp"`
}

var db *dynamodb.Client

//LOGGLY

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
		log.Println("LOGGLY_TOKEN is not set")
		return
	}

	data, err := json.Marshal(entry)
	if err != nil {
		log.Println("Loggly JSON error:", err)
		return
	}

	url := fmt.Sprintf(
		"https://logs-01.loggly.com/inputs/%s/tag/go-http-server/",
		token,
	)

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(data)))
	if err != nil {
		log.Println("Loggly request error:", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Println("Loggly delivery error:", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("Loggly returned HTTP %d", resp.StatusCode)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{
			ResponseWriter: w,
			status:         http.StatusOK,
		}

		next.ServeHTTP(recorder, r)

		logToLoggly(RequestLog{
			Method:     r.Method,
			SourceIP:   sourceIP(r),
			Path:       r.URL.Path,
			StatusCode: recorder.status,
			Timestamp:  time.Now().UTC().Format(time.RFC3339),
		})
	})
}

//json helper functions

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func badRequest(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error": message,
	})
}

func internalError(w http.ResponseWriter, err error) {
	log.Println("Internal server error:", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{
		"error": "Internal server error",
	})
}

// endpoint

// GET /npradhan/status
func statusHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var count int64
	var lastKey map[string]types.AttributeValue

	for {
		input := &dynamodb.ScanInput{
			TableName:         aws.String(tableName),
			Select:            types.SelectCount,
			ExclusiveStartKey: lastKey,
		}

		result, err := db.Scan(ctx, input)
		if err != nil {
			internalError(w, err)
			return
		}

		count += int64(result.Count)
		lastKey = result.LastEvaluatedKey

		if len(lastKey) == 0 {
			break
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"table":       tableName,
		"recordCount": count,
	})
}

// all endpoint

// GET /npradhan/all
func allHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var items []map[string]interface{}
	var lastKey map[string]types.AttributeValue

	for {
		result, err := db.Scan(ctx, &dynamodb.ScanInput{
			TableName:         aws.String(tableName),
			ExclusiveStartKey: lastKey,
		})
		if err != nil {
			internalError(w, err)
			return
		}

		var page []map[string]interface{}
		err = attributevalue.UnmarshalListOfMaps(result.Items, &page)
		if err != nil {
			internalError(w, err)
			return
		}

		items = append(items, page...)
		lastKey = result.LastEvaluatedKey

		if len(lastKey) == 0 {
			break
		}
	}

	writeJSON(w, http.StatusOK, items)
}

// search

var symbolPattern = regexp.MustCompile(`^[A-Z]{1,5}([.-][A-Z])?$`)

func getSingleParam(
	params map[string][]string,
	name string,
) (string, error) {
	values, exists := params[name]
	if !exists {
		return "", nil
	}

	if len(values) != 1 {
		return "", fmt.Errorf("parameter %s must appear only once", name)
	}

	value := strings.TrimSpace(values[0])
	if value == "" {
		return "", fmt.Errorf("parameter %s cannot be empty", name)
	}

	return value, nil
}

func parsePrice(value, name string) (*float64, error) {
	if value == "" {
		return nil, nil
	}

	price, err := strconv.ParseFloat(value, 64)
	if err != nil || price < 0 {
		return nil, fmt.Errorf("%s must be a nonnegative number", name)
	}

	return &price, nil
}

func parseTimestamp(value, name string) (*string, error) {
	if value == "" {
		return nil, nil
	}

	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, fmt.Errorf("%s must be a valid RFC3339 timestamp", name)
	}

	normalized := parsed.UTC().Format("2006-01-02T15:04:05.000000000Z")
	return &normalized, nil
}

// search endpoint

// GET /npradhan/search?Symbol=NVDA
func searchHandler(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()

	// Only Symbol is allowed.
	for name := range params {
		if name != "Symbol" {
			badRequest(w, "Unsupported search parameter: "+name)
			return
		}
	}

	symbol, err := getSingleParam(params, "Symbol")
	if err != nil || symbol == "" {
		badRequest(w, "Symbol is required and must appear only once")
		return
	}

	symbol = strings.ToUpper(symbol)

	if !symbolPattern.MatchString(symbol) {
		badRequest(w, "Symbol must be a valid stock ticker")
		return
	}

	// Search DynamoDB using the ticker symbol.
	keyCondition := expression.Key("Symbol").Equal(
		expression.Value(symbol),
	)

	expr, err := expression.NewBuilder().
		WithKeyCondition(keyCondition).
		Build()

	if err != nil {
		internalError(w, err)
		return
	}

	var items []map[string]interface{}
	var lastKey map[string]types.AttributeValue

	for {
		result, err := db.Query(r.Context(), &dynamodb.QueryInput{
			TableName:                 aws.String(tableName),
			KeyConditionExpression:    expr.KeyCondition(),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			ExclusiveStartKey:         lastKey,
		})

		if err != nil {
			internalError(w, err)
			return
		}

		var page []map[string]interface{}

		err = attributevalue.UnmarshalListOfMaps(result.Items, &page)
		if err != nil {
			internalError(w, err)
			return
		}

		items = append(items, page...)
		lastKey = result.LastEvaluatedKey

		if len(lastKey) == 0 {
			break
		}
	}

	writeJSON(w, http.StatusOK, items)
}

// MAIN

func main() {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}

	awsConfig, err := config.LoadDefaultConfig(
		context.Background(),
		config.WithRegion(region),
	)
	if err != nil {
		log.Fatal("AWS configuration error:", err)
	}

	db = dynamodb.NewFromConfig(awsConfig)

	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}

	router := mux.NewRouter()

	router.HandleFunc("/npradhan/all", allHandler).Methods(http.MethodGet)
	router.HandleFunc("/npradhan/status", statusHandler).Methods(http.MethodGet)
	router.HandleFunc("/npradhan/search", searchHandler).Methods(http.MethodGet)

	router.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "Not Found",
		})
	})

	router.MethodNotAllowedHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Method Not Allowed",
		})
	})

	log.Printf("Server listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, loggingMiddleware(router)))
}

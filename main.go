package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Structs to decode GitHub webhook payload
type WorkflowRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	RunNumber  int    `json:"run_number"`
}

type GitHubWorkflowPayload struct {
	Action      string      `json:"action"`
	WorkflowRun WorkflowRun `json:"workflow_run"`
}

// Prometheus metrics
var (
	runningGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "github_actions_running_total",
			Help: "Number of workflows currently running",
		},
		[]string{"workflow_name"},
	)
	successCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "github_actions_success_total",
			Help: "Total number of successful workflow runs",
		},
		[]string{"workflow_name"},
	)
	failureCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "github_actions_failure_total",
			Help: "Total number of failed workflow runs",
		},
		[]string{"workflow_name"},
	)
	mu sync.Mutex // To safely update metrics
)

func init() {
	// Register Prometheus metrics
	prometheus.MustRegister(runningGauge)
	prometheus.MustRegister(successCounter)
	prometheus.MustRegister(failureCounter)
}

func webhookHandler(c *gin.Context) {
	fmt.Println("\n\n\n\n Running Webhook")
	body, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	fmt.Println("Raw Payload: ", string(body))

	var payload GitHubWorkflowPayload

	// Decode the incoming JSON payload
	if err := c.ShouldBindJSON(&payload); err != nil {
		fmt.Println("error in payload binding: ", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid payload"})
		return
	}

	workflowName := payload.WorkflowRun.Name

	mu.Lock()
	defer mu.Unlock()
	fmt.Println("payload : ", payload)
	switch payload.WorkflowRun.Status {
	case "in_progress":
		// Workflow started running
		runningGauge.WithLabelValues(workflowName).Inc()

	case "completed":
		// Workflow completed: Decrement running
		runningGauge.WithLabelValues(workflowName).Dec()

		if payload.WorkflowRun.Conclusion == "success" {
			successCounter.WithLabelValues(workflowName).Inc()
		} else if payload.WorkflowRun.Conclusion == "failure" {
			failureCounter.WithLabelValues(workflowName).Inc()
		}
	}

	c.String(http.StatusOK, "Event processed")
}

func metricsHandler(c *gin.Context) {
	// Use promhttp to expose metrics
	promhttp.Handler().ServeHTTP(c.Writer, c.Request)
}

func main() {
	r := gin.Default()

	r.POST("/webhook", webhookHandler)
	r.GET("/metrics", metricsHandler)
	r.GET("/readyness", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})
	r.Run(":8080") // listen and serve on 0.0.0.0:8080
}

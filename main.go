package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/shirou/gopsutil/v3/process"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	metricTTL = time.Minute * 1
)

// Structs to decode GitHub webhook payload
type Repository struct {
	FullName string `json:"full_name"`
}

type WorkflowRun struct {
	ID         int       `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	RunNumber  int       `json:"run_number"`
	StartedAt  time.Time `json:"run_started_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Step struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion,omitempty"`
	Number      int       `json:"number"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

type WorkflowJob struct {
	ID           int       `json:"id"`
	RunID        int       `json:"run_id"`
	Name         string    `json:"name"`
	WorkFlowName string    `json:"workflow_name"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion,omitempty"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	CompletedAt  time.Time `json:"completed_at,omitempty"`
	Steps        []Step    `json:"steps,omitempty"`
}

type GitHubWebhookPayload struct {
	Action      string      `json:"action"`
	WorkflowRun WorkflowRun `json:"workflow_run,omitempty"`
	WorkflowJob WorkflowJob `json:"workflow_job,omitempty"`
	Repository  Repository  `json:"repository,omitempty"`
}
type RunInfo struct {
	RunNumber int
	TimeStamp time.Time
}
type metricKey struct {
	metricType string
	labelsKey  string
}

var (
	workflowStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "github_actions_workflow_status",
			Help: "Latest status of workflow runs (1=active, 0=inactive)",
		},
		[]string{"id", "workflow", "repository"},
	)

	workflowRunTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "github_actions_workflow_run_count",
			Help: "Total number of workflow runs per repository",
		},
		[]string{"repository", "workflow"},
	)
	//github_actions_workflow_run_count_success
	workflowRunCountSuccess = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "github_actions_workflow_run_count_success",
			Help: "Total number of success workflow runs",
		},
		[]string{"repository", "workflow"},
	)
	workflowRunCountFailed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "github_actions_workflow_run_count_failed",
			Help: "Total number of failed workflow runs",
		},
		[]string{"repository", "workflow"},
	)

	workflowRunDuration = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "github_actions_workflow_run_duration",
			Help: "Duration of the workflow run in seconds",
		},
		[]string{"id", "workflow", "repository"},
	)

	jobRunTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "github_actions_job_run_total",
			Help: "Total number of jobs run within workflows",
		},
		[]string{"repository", "workflow", "job", "status"},
	)

	jobStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "github_actions_job_status",
			Help: "Latest status of a job (1=active, 0=inactive)",
		},
		[]string{"id", "workflow", "job_name", "repository"},
	)

	jobDuration = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "github_actions_job_run_duration",
			Help: "Job run duration in seconds",
		},
		[]string{"id", "workflow", "job_name", "repository"},
	)

	//stepRunTotal = prometheus.NewCounterVec(
	//	prometheus.CounterOpts{
	//		Name: "github_actions_step_run_total",
	//		Help: "Total number of steps executed",
	//	},
	//	[]string{"repository", "workflow", "job", "step", "status"},
	//)

	//stepDurationSeconds = prometheus.NewHistogramVec(
	//	prometheus.HistogramOpts{
	//		Name: "github_actions_step_duration_seconds",
	//		Help: "Time taken by each individual step",
	//		//Buckets: prometheus.DefBuckets,
	//		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	//	},
	//	[]string{"repository", "workflow", "job", "step", "status"},
	//)

	//queuedDurationSeconds = prometheus.NewHistogramVec(
	//	prometheus.HistogramOpts{
	//		Name:    "github_actions_queued_duration_seconds",
	//		Help:    "Time a workflow spent in queue before starting",
	//		Buckets: prometheus.DefBuckets,
	//	},
	//	[]string{"repository", "workflow"},
	//)

	runnersBusy = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "github_actions_runners_busy",
			Help: "Indicates whether a runner is currently executing a job (1 = busy, 0 = idle)",
		},
		[]string{"repository", "runner_name"},
	)
	testDelete = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "test_delete",
			Help: "testing delete metrics",
		},
		[]string{"repository", "workflow"},
	)
	cpuUsagePercent = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "app_cpu_usage_percent",
			Help: "Current CPU usage percentage of the application",
		},
	)
	memoryUsageBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "app_memory_usage_bytes",
			Help: "Current memory usage in bytes (resident set size)",
		},
	)
	goroutinesCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "app_goroutines_count",
			Help: "Current number of active goroutines",
		},
	)
	mu sync.Mutex

	runIDCache = make(map[int]RunInfo)

	cacheTTL = 30 * time.Minute

	cacheMu  sync.RWMutex
	metricMu sync.RWMutex

	metricTTLMap = make(map[metricKey]struct {
		timeStam time.Time
		labels   []string
	})
)

func makeKey(metricType string, labels []string) metricKey {
	return metricKey{
		metricType: metricType,
		labelsKey:  strings.Join(labels, "|"), // use a delimiter that won't appear in labels
	}
}
func init() {
	// Register Prometheus metrics
	prometheus.MustRegister(
		workflowStatus,
		workflowRunTotal,
		workflowRunCountSuccess,
		workflowRunCountFailed,
		workflowRunDuration,
		jobRunTotal,
		jobDuration,
		//stepRunTotal,
		//stepDurationSeconds,
		//queuedDurationSeconds,
		runnersBusy,
		jobStatus,
		cpuUsagePercent,
		memoryUsageBytes,
		goroutinesCount,
		testDelete,
	)

}
func cleanMetrics(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			slog.Info("Cleaning metrics", "time", now)
			//metricMu.RLock()
			//var toDelete []metricKey
			//for k, v := range metricTTLMap {
			//	if now.After(v.timeStam) {
			//		toDelete = append(toDelete, k)
			//	}
			//}
			//metricMu.RUnlock()
			//
			//metricMu.Lock()
			//for _, metric := range toDelete {
			//	fmt.Println("Metrics deleting expired metric : ", metric.metricType)
			//	val := metricTTLMap[metric]
			//	switch metric.metricType {
			//	case "github_actions_workflow_run_count":
			//		workflowRunTotal.DeleteLabelValues(val.labels...)
			//	case "github_actions_workflow_run_count_success":
			//		workflowRunCountSuccess.DeleteLabelValues(val.labels...)
			//	case "test_delete":
			//		testDelete.DeleteLabelValues(val.labels...)
			//	}
			//	delete(metricTTLMap, metric)
			//
			//}
			//metricMu.Unlock()

			metricMu.Lock()
			for key, val := range metricTTLMap {
				if now.After(val.timeStam) {
					//if now.Sub(val.timeStam) > metricTTL {
					fmt.Println("Metrics deleting expired metric : ", key, now)
					switch key.metricType {
					case "github_actions_workflow_run_count":
						workflowRunTotal.DeleteLabelValues(val.labels...)
					case "github_actions_workflow_run_count_success":
						workflowRunCountSuccess.DeleteLabelValues(val.labels...)
					case "test_delete":
						testDelete.DeleteLabelValues(val.labels...)
					}
					delete(metricTTLMap, key)
				}
			}
			metricMu.Unlock()
		}
	}
}

func cacheCleaner(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			fmt.Println("Metrics ticker started : ", now)

			cacheMu.Lock()
			for k, v := range runIDCache {
				if now.Sub(v.TimeStamp) > cacheTTL {
					delete(runIDCache, k)
				}
			}
			cacheMu.Unlock()
		}
	}
}

type ForRunNumber struct {
	Id        int `json:"id"`
	RunNumber int `json:"run_number"`
}

func fetchRunNumber(ctx context.Context, runID int) (ForRunNumber, error) {

	getUrl := os.Getenv("URL")
	url := getUrl + strconv.Itoa(runID)
	token := os.Getenv("GITHUB_TOKEN")
	fmt.Println("url : ", url, token)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return ForRunNumber{}, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "go-github-client")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return ForRunNumber{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("error in read : ", err)
		return ForRunNumber{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return ForRunNumber{}, fmt.Errorf("GitHub API error: %s\n%s", resp.Status, string(body))
	}

	var run ForRunNumber

	if err := json.Unmarshal(body, &run); err != nil {
		log.Fatalf("Failed to unmarshal JSON: %v", err)
		return ForRunNumber{}, err
	}
	fmt.Printf("Workflow Run ID: %d\n", run.Id)
	fmt.Printf("Status: %d\n", run.RunNumber)

	return run, nil
}
func webhookHandler(c *gin.Context) {
	ctx := c.Request.Context()
	fmt.Println("\n\n\n\n Running Webhook")
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		slog.Error("reading request body", "error", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	fmt.Println("Raw Payload: ", string(body))
	var payload GitHubWebhookPayload

	err = json.Unmarshal(body, &payload)
	if err != nil {
		slog.Error("unmarshalling JSON", "error", err)
		fmt.Println("\n\n error in unmarshal json : ", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		return
	}

	//mu.Lock()
	//defer mu.Unlock()

	if isPayloadOld(payload) {
		slog.Info("discarding old event",
			"workflow_job_id", payload.WorkflowJob.ID,
			"workflow_run_id", payload.WorkflowRun.ID)
		c.Status(http.StatusNoContent)
		return
	}

	if payload.WorkflowRun.ID != 0 {
		cacheMu.Lock()
		runIDCache[payload.WorkflowRun.ID] = RunInfo{
			RunNumber: payload.WorkflowRun.RunNumber,
			TimeStamp: time.Now(),
		}
		cacheMu.Unlock()
	}

	runNumber, err := getRunNumber(ctx, payload)
	if err != nil {
		slog.Error("fetching run number", "error", err)
	}

	if payload.WorkflowJob.ID != 0 {
		processWorkflowJob(payload, runNumber)
	}

	if payload.WorkflowRun.ID != 0 {
		processWorkflowRun(payload)
	}

	c.String(http.StatusOK, "Event processed")
}

func metricsHandler(c *gin.Context) {
	// Use promhttp to expose metrics
	promhttp.Handler().ServeHTTP(c.Writer, c.Request)
}

func processWorkflowJob(payload GitHubWebhookPayload, runNumber int) {
	job := payload.WorkflowJob
	slog.Info("processing workflow job",
		"job_id", job.ID,
		"run_id", job.RunID,
		"name", job.Name,
		"status", job.Status,
		"repo", payload.Repository.FullName)

	jobRunTotal.WithLabelValues(payload.Repository.FullName, strconv.Itoa(runNumber), job.Name, job.Conclusion).Inc()

	if payload.Action == "completed" {
		duration := job.CompletedAt.Sub(job.StartedAt).Seconds()
		jobDuration.WithLabelValues(strconv.Itoa(runNumber), job.WorkFlowName, job.Name, payload.Repository.FullName).Set(duration)
	}
	if payload.Action == "in_progress" {
		runnersBusy.WithLabelValues(payload.Repository.FullName, job.Name).Set(1)
	} else if payload.Action == "completed" {
		runnersBusy.WithLabelValues(payload.Repository.FullName, job.Name).Set(0)
	}

	status := 0.0

	if payload.Action == "queued" {
		status = 0.0
	} else if payload.Action == "in_progress" {
		status = 0.5
	} else if payload.Action == "completed" {
		if job.Conclusion == "success" {
			status = 1.0
		} else if job.Conclusion == "failure" {
			status = 2.0
		}
	}
	jobStatus.WithLabelValues(strconv.Itoa(runNumber), job.WorkFlowName, job.Name, payload.Repository.FullName).Set(status)

	//for _, step := range job.Steps {
	//	stepRunTotal.WithLabelValues(payload.Repository.FullName, strconv.Itoa(runNumber), job.Name, step.Name, step.Status).Inc()
	//	if payload.Action == "completed" {
	//		duration := step.CompletedAt.Sub(step.StartedAt).Seconds()
	//		stepDurationSeconds.WithLabelValues(payload.Repository.FullName, strconv.Itoa(runNumber), job.Name, step.Name, step.Conclusion).Observe(duration)
	//	}
	//}
}
func processWorkflowRun(payload GitHubWebhookPayload) {
	run := payload.WorkflowRun
	slog.Info("processing workflow run",
		"job_id", run.ID,
		"run_number", run.RunNumber,
		"name", run.Name,
		"status", run.Status,
		"repo", payload.Repository.FullName)

	workflowRunTotal.WithLabelValues(payload.Repository.FullName, strconv.Itoa(run.RunNumber)).Inc()
	metricMu.Lock()
	metricTTLMap[makeKey("github_actions_workflow_run_count", []string{payload.Repository.FullName, strconv.Itoa(run.RunNumber)})] = struct {
		timeStam time.Time
		labels   []string
	}{timeStam: time.Now().Add(1 * time.Hour),
		labels: []string{payload.Repository.FullName, strconv.Itoa(run.RunNumber)},
	}
	metricMu.Unlock()

	if run.Conclusion == "success" {
		workflowRunCountSuccess.WithLabelValues(payload.Repository.FullName, run.Name).Inc()
		metricMu.Lock()
		metricTTLMap[makeKey("github_actions_workflow_run_count_success", []string{payload.Repository.FullName, run.Name})] = struct {
			timeStam time.Time
			labels   []string
		}{timeStam: time.Now().Add(1 * time.Hour),
			labels: []string{payload.Repository.FullName, run.Name},
		}
		metricMu.Unlock()
	} else if run.Conclusion == "failure" {
		workflowRunCountFailed.WithLabelValues(payload.Repository.FullName, run.Name).Inc()
		metricMu.Lock()
		metricTTLMap[makeKey("github_actions_workflow_run_count_failed", []string{payload.Repository.FullName, run.Name})] = struct {
			timeStam time.Time
			labels   []string
		}{timeStam: time.Now().Add(1 * time.Hour),
			labels: []string{payload.Repository.FullName, run.Name},
		}
		metricMu.Unlock()
	}

	if payload.Action == "completed" {
		duration := run.UpdatedAt.Sub(run.StartedAt).Seconds()
		workflowRunDuration.WithLabelValues(strconv.Itoa(run.RunNumber), run.Name, payload.Repository.FullName).Set(duration)
	}

	status := 0.0

	if payload.Action == "requested" {
		status = 0.0
	} else if payload.Action == "in_progress" {
		status = 0.5
	} else if payload.Action == "completed" {
		if run.Conclusion == "success" {
			status = 1.0
		} else if run.Conclusion == "failure" {
			status = 2.0
		}
	}
	workflowStatus.WithLabelValues(strconv.Itoa(run.RunNumber), run.Name, payload.Repository.FullName).Set(status)
}
func getRunNumber(ctx context.Context, payload GitHubWebhookPayload) (int, error) {
	cacheMu.RLock()
	if info, ok := runIDCache[payload.WorkflowJob.RunID]; ok {
		cacheMu.RUnlock()
		return info.RunNumber, nil
	}
	cacheMu.RUnlock()

	run, err := fetchRunNumber(ctx, payload.WorkflowJob.RunID)
	if err != nil {
		return 0, err
	}
	return run.RunNumber, nil
}
func isPayloadOld(payload GitHubWebhookPayload) bool {
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	return (payload.WorkflowJob.ID != 0 && !payload.WorkflowJob.CompletedAt.IsZero() && payload.WorkflowJob.CompletedAt.Before(cutoff)) ||
		(payload.WorkflowRun.ID != 0 && !payload.WorkflowRun.UpdatedAt.IsZero() && payload.WorkflowRun.UpdatedAt.Before(cutoff))
}

func monitorResourceUsage(ctx context.Context) {
	p, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		slog.Error("Failed to initialize process for monitoring", "error", err)
		return
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fmt.Println("Memory ticker ")
			// CPU usage
			cpuPercent, err := p.CPUPercent()
			if err != nil {
				slog.Error("Failed to get CPU usage", "error", err)
			} else {
				cpuUsagePercent.Set(cpuPercent)
			}

			// Memory usage
			memInfo, err := p.MemoryInfo()

			if err != nil {
				slog.Error("Failed to get memory usage", "error", err)
			} else {
				memoryUsageBytes.Set(float64(memInfo.RSS))
			}

			// Goroutine count
			goroutinesCount.Set(float64(runtime.NumGoroutine()))

			slog.Debug("Resource usage updated",
				"cpu_percent", cpuPercent,
				"memory_bytes", memInfo.RSS,
				"goroutines", runtime.NumGoroutine())
		}
	}
}
func main() {

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	r := gin.Default()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go cleanMetrics(ctx)
	go cacheCleaner(ctx)
	go monitorResourceUsage(ctx)

	r.POST("/webhook", webhookHandler)
	r.GET("/metrics", metricsHandler)

	r.GET("/readiness", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	r.GET("/get_number", func(c *gin.Context) {
		if len(runIDCache) != 0 {
			for i, v := range runIDCache {
				fmt.Println("runID : ", i, "run Number : ", v)
				c.JSON(http.StatusOK, gin.H{"runID : ": i, " runNumber : ": v})
			}
		} else {
			c.Status(http.StatusNoContent)
		}
		//for i := 1; i <= 3; i++ {
		//label := "label_" + strconv.Itoa(i)
		//testDelete.WithLabelValues("Abhishek_repo", label).Inc()

		//metricMu.Lock()
		//
		//metricTTLMap[makeKey("test_delete", []string{"Abhishek_repo", label})] = struct {
		//	timeStam time.Time
		//	labels   []string
		//}{timeStam: time.Now().Add(10 * time.Second),
		//	labels: []string{"Abhishek_repo", label},
		//}
		//metricMu.Unlock()
		//}

	})
	if err := r.Run(":8080"); err != nil {
		fmt.Println("error in port : ", err)
		slog.Error("starting server", "error", err)
		os.Exit(1)
	}

}

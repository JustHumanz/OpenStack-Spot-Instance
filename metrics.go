package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

type Metrics interface {
	Loads(context.Context, []Host) (map[string]float64, error)
}

type placementMetrics struct{}

func (placementMetrics) Loads(_ context.Context, hosts []Host) (map[string]float64, error) {
	result := map[string]float64{}
	for _, host := range hosts {
		inventory, exists := host.Inventory["VCPU"]
		capacity := inventory.Available(0)
		if exists && capacity > 0 {
			result[host.Name] = 100 * float64(host.Usage["VCPU"]) / float64(capacity)
		}
	}
	return result, nil
}

type prometheusMetrics struct {
	settings config
	client   *http.Client
}

func (driver prometheusMetrics) Loads(ctx context.Context, hosts []Host) (map[string]float64, error) {
	endpoint, err := url.Parse(driver.settings.PrometheusURL)
	if err != nil {
		return nil, err
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, fmt.Errorf("invalid Prometheus URL scheme")
	}
	if endpoint.Path == "/query" || endpoint.Path == "/api/v1/query" {
		endpoint.Path = ""
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/v1/query"
	query := endpoint.Query()
	query.Set("query", driver.settings.PrometheusQuery)
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := driver.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Prometheus returned %s", response.Status)
	}
	var payload struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if payload.Status != "success" || payload.Data.ResultType != "vector" {
		return nil, fmt.Errorf("invalid Prometheus response: %s", payload.Error)
	}
	values := map[string]float64{}
	duplicates := map[string]bool{}
	for _, sample := range payload.Data.Result {
		label := sample.Metric["instance"]
		if label == "" || len(sample.Value) != 2 {
			continue
		}
		var timestamp float64
		var text string
		if json.Unmarshal(sample.Value[0], &timestamp) != nil || json.Unmarshal(sample.Value[1], &text) != nil {
			continue
		}
		value, err := strconv.ParseFloat(text, 64)
		age := time.Since(time.Unix(int64(timestamp), 0))
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 || age > driver.settings.MaxAge || age < -time.Minute {
			continue
		}
		if _, exists := values[label]; exists {
			duplicates[label] = true
		}
		values[label] = value
	}
	result := map[string]float64{}
	labelsByIP := map[string][]string{}
	for label := range values {
		address, _, err := net.SplitHostPort(label)
		if err != nil {
			address = label
		}
		if ip := net.ParseIP(address); ip != nil {
			labelsByIP[ip.String()] = append(labelsByIP[ip.String()], label)
		}
	}
	hostsByIP := map[string]int{}
	for _, host := range hosts {
		if ip := net.ParseIP(host.HostIP); ip != nil {
			hostsByIP[ip.String()]++
		}
	}
	for _, host := range hosts {
		label := metricName(driver.settings, host.Name)
		if value, ok := values[label]; ok && !duplicates[label] {
			result[host.Name] = value
			continue
		}
		if driver.settings.HostMap[host.Name] != "" {
			continue
		}
		ip := net.ParseIP(host.HostIP)
		if ip == nil || hostsByIP[ip.String()] != 1 || len(labelsByIP[ip.String()]) != 1 {
			continue
		}
		label = labelsByIP[ip.String()][0]
		if !duplicates[label] {
			result[host.Name] = values[label]
			log.Printf("matched Prometheus instance=%s to host=%s using Nova host_ip", label, host.Name)
		}
	}
	return result, nil
}

type kubernetesMetrics struct {
	settings config
	core     kubernetes.Interface
	metrics  metricsclient.Interface
}

func (driver kubernetesMetrics) Loads(ctx context.Context, hosts []Host) (map[string]float64, error) {
	result := map[string]float64{}
	for _, host := range hosts {
		name := metricName(driver.settings, host.Name)
		node, err := driver.core.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		sample, err := driver.metrics.MetricsV1beta1().NodeMetricses().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		capacity := node.Status.Capacity.Cpu().MilliValue()
		cpu := sample.Usage.Cpu().MilliValue()
		age := time.Since(sample.Timestamp.Time)
		if capacity <= 0 || cpu < 0 || age > driver.settings.MaxAge || age < -time.Minute {
			continue
		}
		result[host.Name] = 100 * float64(cpu) / float64(capacity)
	}
	return result, nil
}

func metricName(settings config, host string) string {
	if name := settings.HostMap[host]; name != "" {
		return name
	}
	return host
}

func newMetrics(settings config) (Metrics, error) {
	switch settings.Driver {
	case "placement":
		return placementMetrics{}, nil
	case "prometheus":
		return prometheusMetrics{settings: settings, client: &http.Client{Timeout: 15 * time.Second}}, nil
	case "metrics-server":
		var kubeConfig *rest.Config
		var err error

		if path := settings.Kubeconfig; path != "" {
			kubeConfig, err = clientcmd.BuildConfigFromFlags("", path)
		} else {
			kubeConfig, err = rest.InClusterConfig()
		}
		if err != nil {
			return nil, fmt.Errorf("Kubernetes configuration: %w", err)
		}
		core, err := kubernetes.NewForConfig(kubeConfig)
		if err != nil {
			return nil, err
		}
		metrics, err := metricsclient.NewForConfig(kubeConfig)
		if err != nil {
			return nil, err
		}
		return kubernetesMetrics{settings: settings, core: core, metrics: metrics}, nil
	default:
		return nil, fmt.Errorf("unknown metrics driver %s", settings.Driver)
	}
}

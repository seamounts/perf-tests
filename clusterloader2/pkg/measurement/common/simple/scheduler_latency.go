/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package simple

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/prometheus/common/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/master/ports"
	schedulermetric "k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/util/system"
	"k8s.io/perf-tests/clusterloader2/pkg/measurement"
	measurementutil "k8s.io/perf-tests/clusterloader2/pkg/measurement/util"
	"k8s.io/perf-tests/clusterloader2/pkg/util"
)

const (
	schedulerLatencyMetricName  = "SchedulingMetrics"
	schedulingLatencyMetricName = model.LabelValue(schedulermetric.SchedulerSubsystem + "_" + schedulermetric.SchedulingLatencyName)
	singleRestCallTimeout       = 5 * time.Minute
)

func init() {
	measurement.Register(schedulerLatencyMetricName, createSchedulerLatencyMeasurement)
}

func createSchedulerLatencyMeasurement() measurement.Measurement {
	return &schedulerLatencyMeasurement{}
}

type schedulerLatencyMeasurement struct{}

// Execute supports two actions:
// - reset - Resets latency data on api scheduler side.
// - gather - Gathers and prints current scheduler latency data.
func (s *schedulerLatencyMeasurement) Execute(config *measurement.MeasurementConfig) ([]measurement.Summary, error) {
	var summaries []measurement.Summary
	action, err := util.GetString(config.Params, "action")
	if err != nil {
		return summaries, err
	}
	provider, err := util.GetStringOrDefault(config.Params, "provider", config.ClusterConfig.Provider)
	if err != nil {
		return summaries, err
	}
	masterIP, err := util.GetStringOrDefault(config.Params, "masterIP", config.ClusterConfig.MasterIP)
	if err != nil {
		return summaries, err
	}
	masterName, err := util.GetStringOrDefault(config.Params, "masterName", config.ClusterConfig.MasterName)
	if err != nil {
		return summaries, err
	}

	switch action {
	case "reset":
		glog.Infof("%s: resetting latency metrics in scheduler...", s)
		return summaries, s.resetSchedulerMetrics(config.ClientSets.GetClient(), masterIP, provider, masterName)
	case "gather":
		return s.getSchedulingLatency(config.ClientSets.GetClient(), masterIP, provider, masterName)
	default:
		return summaries, fmt.Errorf("unknown action %v", action)
	}
}

// Dispose cleans up after the measurement.
func (*schedulerLatencyMeasurement) Dispose() {}

// String returns string representation of this measurement.
func (*schedulerLatencyMeasurement) String() string {
	return schedulerLatencyMetricName
}

func (s *schedulerLatencyMeasurement) resetSchedulerMetrics(c clientset.Interface, host, provider, masterName string) error {
	_, err := s.sendRequestToScheduler(c, "DELETE", host, provider, masterName)
	if err != nil {
		return err
	}
	return nil
}

// Retrieves scheduler latency metrics.
func (s *schedulerLatencyMeasurement) getSchedulingLatency(c clientset.Interface, host, provider, masterName string) ([]measurement.Summary, error) {
	var summaries []measurement.Summary
	result := schedulingMetrics{}
	data, err := s.sendRequestToScheduler(c, "GET", host, provider, masterName)
	if err != nil {
		return summaries, err
	}

	samples, err := measurementutil.ExtractMetricSamples(data)
	if err != nil {
		return summaries, err
	}

	for _, sample := range samples {
		if sample.Metric[model.MetricNameLabel] != schedulingLatencyMetricName {
			continue
		}

		var metric *measurementutil.LatencyMetric
		switch sample.Metric[schedulermetric.OperationLabel] {
		case schedulermetric.PredicateEvaluation:
			metric = &result.PredicateEvaluationLatency
		case schedulermetric.PriorityEvaluation:
			metric = &result.PriorityEvaluationLatency
		case schedulermetric.PreemptionEvaluation:
			metric = &result.PreemptionEvaluationLatency
		case schedulermetric.Binding:
			metric = &result.BindingLatency
		}
		if metric == nil {
			continue
		}

		quantile, err := strconv.ParseFloat(string(sample.Metric[model.QuantileLabel]), 64)
		if err != nil {
			return summaries, err
		}
		metric.SetQuantile(quantile, time.Duration(int64(float64(sample.Value)*float64(time.Second))))
	}
	summaries = append(summaries, &result)

	return summaries, nil
}

// Sends request to kube scheduler metrics
func (s *schedulerLatencyMeasurement) sendRequestToScheduler(c clientset.Interface, op, host, provider, masterName string) (string, error) {
	opUpper := strings.ToUpper(op)
	if opUpper != "GET" && opUpper != "DELETE" {
		return "", fmt.Errorf("unknown REST request")
	}

	nodes, err := c.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return "", err
	}

	var masterRegistered = false
	for _, node := range nodes.Items {
		role, _ := node.Labels["kubernetes.io/role"]
		if system.IsMasterNode(node.Name) || strings.Contains(role, "master") {
			masterRegistered = true
			break
		}
	}
	var responseText string
	if masterRegistered {
		ctx, cancel := context.WithTimeout(context.Background(), singleRestCallTimeout)
		defer cancel()

		body, err := c.CoreV1().RESTClient().Verb(opUpper).
			Context(ctx).
			Namespace(metav1.NamespaceSystem).
			Resource("pods").
			Name(fmt.Sprintf("kube-scheduler-%v:%v", masterName, ports.SchedulerPort)).
			SubResource("proxy").
			Suffix("metrics").
			Do().Raw()

		if err != nil {
			return "", err
		}
		responseText = string(body)
	} else {
		// If master is not registered fall back to old method of using SSH.
		if provider == "gke" {
			glog.Infof("%s: not grabbing scheduler metrics through master SSH: unsupported for gke", s)
			return "", nil
		}

		cmd := "curl -X " + opUpper + " http://localhost:10251/metrics"
		sshResult, err := measurementutil.SSH(cmd, host+":22", provider)
		if err != nil || sshResult.Code != 0 {
			return "", fmt.Errorf("unexpected error (code: %d) in ssh connection to master: %#v", sshResult.Code, err)
		}
		responseText = sshResult.Stdout
	}
	return responseText, nil
}

type schedulingMetrics struct {
	PredicateEvaluationLatency  measurementutil.LatencyMetric `json:"predicateEvaluationLatency"`
	PriorityEvaluationLatency   measurementutil.LatencyMetric `json:"priorityEvaluationLatency"`
	PreemptionEvaluationLatency measurementutil.LatencyMetric `json:"preemptionEvaluationLatency"`
	BindingLatency              measurementutil.LatencyMetric `json:"bindingLatency"`
}

// SummaryName returns name of the summary.
func (l *schedulingMetrics) SummaryName() string {
	return schedulerLatencyMetricName
}

// PrintSummary returns summary as a string.
func (l *schedulingMetrics) PrintSummary() (string, error) {
	return util.PrettyPrintJSON(l)
}

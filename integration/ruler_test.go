//go:build integration_ruler
// +build integration_ruler

package integration

import (
	"bytes"
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cortexproject/cortex/pkg/ruler"

	"github.com/cortexproject/cortex/pkg/storage/tsdb"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/rulefmt"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/prompb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thanos-io/objstore/providers/s3"
	"gopkg.in/yaml.v3"

	"github.com/cortexproject/cortex/integration/ca"
	"github.com/cortexproject/cortex/integration/e2e"
	e2edb "github.com/cortexproject/cortex/integration/e2e/db"
	"github.com/cortexproject/cortex/integration/e2ecortex"
)

func TestRulerAPI(t *testing.T) {
	var (
		namespaceOne = "test_/encoded_+namespace/?"
		namespaceTwo = "test_/encoded_+namespace/?/two"
	)
	ruleGroup := createTestRuleGroup(t)

	for _, thanosEngine := range []bool{false, true} {
		t.Run(fmt.Sprintf("thanosEngine=%t", thanosEngine), func(t *testing.T) {
			s, err := e2e.NewScenario(networkName)
			require.NoError(t, err)
			defer s.Close()

			// Start dependencies.
			consul := e2edb.NewConsul()
			minio := e2edb.NewMinio(9000, rulestoreBucketName, bucketName)
			require.NoError(t, s.StartAndWaitReady(consul, minio))

			flags := mergeFlags(BlocksStorageFlags(), RulerFlags(), map[string]string{
				"-querier.thanos-engine": strconv.FormatBool(thanosEngine),
			})

			// Start Cortex components.
			ruler := e2ecortex.NewRuler("ruler", consul.NetworkHTTPEndpoint(), flags, "")
			require.NoError(t, s.StartAndWaitReady(ruler))

			// Create a client with the ruler address configured
			c, err := e2ecortex.NewClient("", "", "", ruler.HTTPEndpoint(), "user-1")
			require.NoError(t, err)

			// Set the rule group into the ruler
			require.NoError(t, c.SetRuleGroup(ruleGroup, namespaceOne))

			// Wait until the user manager is created
			require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(1), "cortex_ruler_managers_total"))

			// Check to ensure the rules running in the ruler match what was set
			rgs, err := c.GetRuleGroups()
			require.NoError(t, err)

			retrievedNamespace, exists := rgs[namespaceOne]
			require.True(t, exists)
			require.Len(t, retrievedNamespace, 1)
			require.Equal(t, retrievedNamespace[0].Name, ruleGroup.Name)

			// Add a second rule group with a similar namespace
			require.NoError(t, c.SetRuleGroup(ruleGroup, namespaceTwo))
			require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(2), "cortex_prometheus_rule_group_rules"))

			// Check to ensure the rules running in the ruler match what was set
			rgs, err = c.GetRuleGroups()
			require.NoError(t, err)

			retrievedNamespace, exists = rgs[namespaceOne]
			require.True(t, exists)
			require.Len(t, retrievedNamespace, 1)
			require.Equal(t, retrievedNamespace[0].Name, ruleGroup.Name)

			retrievedNamespace, exists = rgs[namespaceTwo]
			require.True(t, exists)
			require.Len(t, retrievedNamespace, 1)
			require.Equal(t, retrievedNamespace[0].Name, ruleGroup.Name)

			// Test compression by inspecting the response Headers
			req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/api/prom/rules", ruler.HTTPEndpoint()), nil)
			require.NoError(t, err)

			req.Header.Set("X-Scope-OrgID", "user-1")
			req.Header.Set("Accept-Encoding", "gzip")

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			// Execute HTTP request
			res, err := http.DefaultClient.Do(req.WithContext(ctx))
			require.NoError(t, err)

			defer res.Body.Close()
			// We assert on the Vary header as the minimum response size for enabling compression is 1500 bytes.
			// This is enough to know whenever the handler for compression is enabled or not.
			require.Equal(t, "Accept-Encoding", res.Header.Get("Vary"))

			// Delete the set rule groups
			require.NoError(t, c.DeleteRuleGroup(namespaceOne, ruleGroup.Name))
			require.NoError(t, c.DeleteRuleNamespace(namespaceTwo))

			// Get the rule group and ensure it returns a 404
			resp, err := c.GetRuleGroup(namespaceOne, ruleGroup.Name)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusNotFound, resp.StatusCode)

			// Wait until the users manager has been terminated
			require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(0), "cortex_ruler_managers_total"))

			// Check to ensure the rule groups are no longer active
			_, err = c.GetRuleGroups()
			require.Error(t, err)

			// Ensure no service-specific metrics prefix is used by the wrong service.
			assertServiceMetricsPrefixes(t, Ruler, ruler)
		})
	}
}

func TestRulerAPISingleBinary(t *testing.T) {
	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	namespace := "ns"
	user := "fake"

	configOverrides := map[string]string{
		"-ruler-storage.local.directory": filepath.Join(e2e.ContainerSharedDir, "ruler_configs"),
		"-ruler.poll-interval":           "2s",
		"-ruler.rule-path":               filepath.Join(e2e.ContainerSharedDir, "rule_tmp/"),
	}

	// Start Cortex components.
	require.NoError(t, copyFileToSharedDir(s, "docs/configuration/single-process-config-blocks-local.yaml", cortexConfigFile))
	require.NoError(t, writeFileToSharedDir(s, filepath.Join("ruler_configs", user, namespace), []byte(cortexRulerUserConfigYaml)))
	cortex := e2ecortex.NewSingleBinaryWithConfigFile("cortex", cortexConfigFile, configOverrides, "", 9009, 9095)
	require.NoError(t, s.StartAndWaitReady(cortex))

	// Create a client with the ruler address configured
	c, err := e2ecortex.NewClient("", "", "", cortex.HTTPEndpoint(), "")
	require.NoError(t, err)

	// Wait until the user manager is created
	require.NoError(t, cortex.WaitSumMetrics(e2e.Equals(1), "cortex_ruler_managers_total"))

	// Check to ensure the rules running in the cortex match what was set
	rgs, err := c.GetRuleGroups()
	require.NoError(t, err)

	retrievedNamespace, exists := rgs[namespace]
	require.True(t, exists)
	require.Len(t, retrievedNamespace, 1)
	require.Equal(t, retrievedNamespace[0].Name, "rule")

	// Check to make sure prometheus engine metrics are available for both engine types
	require.NoError(t, cortex.WaitSumMetricsWithOptions(e2e.Equals(0), []string{"prometheus_engine_queries"}, e2e.WithLabelMatchers(
		labels.MustNewMatcher(labels.MatchEqual, "engine", "querier"))))

	require.NoError(t, cortex.WaitSumMetricsWithOptions(e2e.Equals(0), []string{"prometheus_engine_queries"}, e2e.WithLabelMatchers(
		labels.MustNewMatcher(labels.MatchEqual, "engine", "ruler"))))

	// Test Cleanup and Restart

	// Stop the running cortex
	require.NoError(t, cortex.Stop())

	// Restart Cortex with identical configs
	cortexRestarted := e2ecortex.NewSingleBinaryWithConfigFile("cortex-restarted", cortexConfigFile, configOverrides, "", 9009, 9095)
	require.NoError(t, s.StartAndWaitReady(cortexRestarted))

	// Wait until the user manager is created
	require.NoError(t, cortexRestarted.WaitSumMetrics(e2e.Equals(1), "cortex_ruler_managers_total"))
}

func TestRulerEvaluationDelay(t *testing.T) {
	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	namespace := "ns"
	user := "fake"

	evaluationDelay := time.Minute * 5

	configOverrides := map[string]string{
		"-ruler-storage.local.directory":   filepath.Join(e2e.ContainerSharedDir, "ruler_configs"),
		"-ruler.poll-interval":             "2s",
		"-ruler.rule-path":                 filepath.Join(e2e.ContainerSharedDir, "rule_tmp/"),
		"-ruler.evaluation-delay-duration": evaluationDelay.String(),
	}

	// Start Cortex components.
	require.NoError(t, copyFileToSharedDir(s, "docs/configuration/single-process-config-blocks-local.yaml", cortexConfigFile))
	require.NoError(t, writeFileToSharedDir(s, filepath.Join("ruler_configs", user, namespace), []byte(cortexRulerEvalStaleNanConfigYaml)))
	cortex := e2ecortex.NewSingleBinaryWithConfigFile("cortex", cortexConfigFile, configOverrides, "", 9009, 9095)
	require.NoError(t, s.StartAndWaitReady(cortex))

	// Create a client with the ruler address configured
	c, err := e2ecortex.NewClient(cortex.HTTPEndpoint(), cortex.HTTPEndpoint(), "", cortex.HTTPEndpoint(), "")
	require.NoError(t, err)

	now := time.Now()

	// Generate series that includes stale nans
	samplesToSend := 10
	series := prompb.TimeSeries{
		Labels: []prompb.Label{
			{Name: "__name__", Value: "a_sometimes_stale_nan_series"},
			{Name: "instance", Value: "sometimes-stale"},
		},
	}
	series.Samples = make([]prompb.Sample, samplesToSend)
	posStale := 2

	// Create samples, that are delayed by the evaluation delay with increasing values.
	for pos := range series.Samples {
		series.Samples[pos].Timestamp = e2e.TimeToMilliseconds(now.Add(-evaluationDelay).Add(time.Duration(pos) * time.Second))
		series.Samples[pos].Value = float64(pos + 1)

		// insert staleness marker at the positions marked by posStale
		if pos == posStale {
			series.Samples[pos].Value = math.Float64frombits(value.StaleNaN)
		}
	}

	// Insert metrics
	res, err := c.Push([]prompb.TimeSeries{series})
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)

	// Get number of rule evaluations just after push
	ruleEvaluationsAfterPush, err := cortex.SumMetrics([]string{"cortex_prometheus_rule_evaluations_total"})
	require.NoError(t, err)

	// Wait until the rule is evaluated for the first time
	require.NoError(t, cortex.WaitSumMetrics(e2e.Greater(ruleEvaluationsAfterPush[0]), "cortex_prometheus_rule_evaluations_total"))

	// Query the timestamp of the latest result to ensure the evaluation is delayed
	result, err := c.Query("timestamp(stale_nan_eval)", now)
	require.NoError(t, err)
	require.Equal(t, model.ValVector, result.Type())

	vector := result.(model.Vector)
	require.Equal(t, 1, vector.Len(), "expect one sample returned")

	// 290 seconds gives 10 seconds of slack between the rule evaluation and the query
	// to account for CI latency, but ensures the latest evaluation was in the past.
	var maxDiff int64 = 290_000
	require.GreaterOrEqual(t, e2e.TimeToMilliseconds(time.Now())-int64(vector[0].Value)*1000, maxDiff)

	// Wait until all the pushed samples have been evaluated by the rule. This
	// ensures that rule results are successfully written even after a
	// staleness period.
	require.NoError(t, cortex.WaitSumMetrics(e2e.GreaterOrEqual(ruleEvaluationsAfterPush[0]+float64(samplesToSend)), "cortex_prometheus_rule_evaluations_total"))

	// query all results to verify rules have been evaluated correctly
	result, err = c.QueryRange("stale_nan_eval", now.Add(-evaluationDelay), now, time.Second)
	require.NoError(t, err)
	require.Equal(t, model.ValMatrix, result.Type())

	matrix := result.(model.Matrix)
	require.GreaterOrEqual(t, 1, matrix.Len(), "expect at least a series returned")

	// Iterate through the values recorded and ensure they exist as expected.
	inputPos := 0
	for _, m := range matrix {
		for _, v := range m.Values {
			// Skip values for stale positions
			if inputPos == posStale {
				inputPos++
			}

			expectedValue := model.SampleValue(2 * (inputPos + 1))
			require.Equal(t, expectedValue, v.Value)

			// Look for next value
			inputPos++

			// We have found all input values
			if inputPos >= len(series.Samples) {
				break
			}
		}
	}
	require.Equal(t, len(series.Samples), inputPos, "expect to have returned all evaluations")
}

func TestRulerSharding(t *testing.T) {
	const numRulesGroups = 100

	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	// Generate multiple rule groups, with 1 rule each.
	ruleGroups := make([]rulefmt.RuleGroup, numRulesGroups)
	expectedNames := make([]string, numRulesGroups)
	for i := 0; i < numRulesGroups; i++ {
		var recordNode yaml.Node
		var exprNode yaml.Node

		recordNode.SetString(fmt.Sprintf("rule_%d", i))
		exprNode.SetString(strconv.Itoa(i))
		ruleName := fmt.Sprintf("test_%d", i)

		expectedNames[i] = ruleName
		ruleGroups[i] = rulefmt.RuleGroup{
			Name:     ruleName,
			Interval: 60,
			Rules: []rulefmt.RuleNode{{
				Record: recordNode,
				Expr:   exprNode,
			}},
		}
	}

	// Start dependencies.
	consul := e2edb.NewConsul()
	minio := e2edb.NewMinio(9000, rulestoreBucketName)
	require.NoError(t, s.StartAndWaitReady(consul, minio))

	// Configure the ruler.
	rulerFlags := mergeFlags(
		BlocksStorageFlags(),
		RulerFlags(),
		RulerShardingFlags(consul.NetworkHTTPEndpoint()),
		map[string]string{
			// Since we're not going to run any rule, we don't need the
			// store-gateway to be configured to a valid address.
			"-querier.store-gateway-addresses": "localhost:12345",
			// Enable the bucket index so we can skip the initial bucket scan.
			"-blocks-storage.bucket-store.bucket-index.enabled": "true",
		},
	)

	// Start rulers.
	ruler1 := e2ecortex.NewRuler("ruler-1", consul.NetworkHTTPEndpoint(), rulerFlags, "")
	ruler2 := e2ecortex.NewRuler("ruler-2", consul.NetworkHTTPEndpoint(), rulerFlags, "")
	rulers := e2ecortex.NewCompositeCortexService(ruler1, ruler2)
	require.NoError(t, s.StartAndWaitReady(ruler1, ruler2))

	// Upload rule groups to one of the rulers.
	c, err := e2ecortex.NewClient("", "", "", ruler1.HTTPEndpoint(), "user-1")
	require.NoError(t, err)

	for _, ruleGroup := range ruleGroups {
		require.NoError(t, c.SetRuleGroup(ruleGroup, "test"))
	}

	// Wait until rulers have loaded all rules.
	require.NoError(t, rulers.WaitSumMetricsWithOptions(e2e.Equals(numRulesGroups), []string{"cortex_prometheus_rule_group_rules"}, e2e.WaitMissingMetrics))

	// Since rulers have loaded all rules, we expect that rules have been sharded
	// between the two rulers.
	require.NoError(t, ruler1.WaitSumMetrics(e2e.Less(numRulesGroups), "cortex_prometheus_rule_group_rules"))
	require.NoError(t, ruler2.WaitSumMetrics(e2e.Less(numRulesGroups), "cortex_prometheus_rule_group_rules"))

	// Fetch the rules and ensure they match the configured ones.
	actualGroups, err := c.GetPrometheusRules(e2ecortex.DefaultFilter)
	require.NoError(t, err)

	var actualNames []string
	for _, group := range actualGroups {
		actualNames = append(actualNames, group.Name)
	}
	assert.ElementsMatch(t, expectedNames, actualNames)
}

func TestRulerAPISharding(t *testing.T) {
	const numRulesGroups = 100

	random := rand.New(rand.NewSource(time.Now().UnixNano()))
	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	// Generate multiple rule groups, with 1 rule each.
	ruleGroups := make([]rulefmt.RuleGroup, numRulesGroups)
	expectedNames := make([]string, numRulesGroups)
	alertCount := 0
	for i := 0; i < numRulesGroups; i++ {
		num := random.Intn(100)
		var ruleNode yaml.Node
		var exprNode yaml.Node

		ruleNode.SetString(fmt.Sprintf("rule_%d", i))
		exprNode.SetString(strconv.Itoa(i))
		ruleName := fmt.Sprintf("test_%d", i)

		expectedNames[i] = ruleName
		if num%2 == 0 {
			alertCount++
			ruleGroups[i] = rulefmt.RuleGroup{
				Name:     ruleName,
				Interval: 60,
				Rules: []rulefmt.RuleNode{{
					Alert: ruleNode,
					Expr:  exprNode,
				}},
			}
		} else {
			ruleGroups[i] = rulefmt.RuleGroup{
				Name:     ruleName,
				Interval: 60,
				Rules: []rulefmt.RuleNode{{
					Record: ruleNode,
					Expr:   exprNode,
				}},
			}
		}
	}

	// Start dependencies.
	consul := e2edb.NewConsul()
	minio := e2edb.NewMinio(9000, rulestoreBucketName)
	require.NoError(t, s.StartAndWaitReady(consul, minio))

	// Configure the ruler.
	rulerFlags := mergeFlags(
		BlocksStorageFlags(),
		RulerFlags(),
		RulerShardingFlags(consul.NetworkHTTPEndpoint()),
		map[string]string{
			// Since we're not going to run any rule, we don't need the
			// store-gateway to be configured to a valid address.
			"-querier.store-gateway-addresses": "localhost:12345",
			// Enable the bucket index so we can skip the initial bucket scan.
			"-blocks-storage.bucket-store.bucket-index.enabled": "true",
		},
	)

	// Start rulers.
	ruler1 := e2ecortex.NewRuler("ruler-1", consul.NetworkHTTPEndpoint(), rulerFlags, "")
	ruler2 := e2ecortex.NewRuler("ruler-2", consul.NetworkHTTPEndpoint(), rulerFlags, "")
	rulers := e2ecortex.NewCompositeCortexService(ruler1, ruler2)
	require.NoError(t, s.StartAndWaitReady(ruler1, ruler2))

	// Upload rule groups to one of the rulers.
	c, err := e2ecortex.NewClient("", "", "", ruler1.HTTPEndpoint(), "user-1")
	require.NoError(t, err)

	namespaceNames := []string{"test1", "test2", "test3", "test4", "test5"}
	namespaceNameCount := make([]int, 5)
	nsRand := rand.New(rand.NewSource(time.Now().UnixNano()))
	for _, ruleGroup := range ruleGroups {
		index := nsRand.Intn(len(namespaceNames))
		namespaceNameCount[index] = namespaceNameCount[index] + 1
		require.NoError(t, c.SetRuleGroup(ruleGroup, namespaceNames[index]))
	}

	// Wait until rulers have loaded all rules.
	require.NoError(t, rulers.WaitSumMetricsWithOptions(e2e.Equals(numRulesGroups), []string{"cortex_prometheus_rule_group_rules"}, e2e.WaitMissingMetrics))

	// Since rulers have loaded all rules, we expect that rules have been sharded
	// between the two rulers.
	require.NoError(t, ruler1.WaitSumMetrics(e2e.Less(numRulesGroups), "cortex_prometheus_rule_group_rules"))
	require.NoError(t, ruler2.WaitSumMetrics(e2e.Less(numRulesGroups), "cortex_prometheus_rule_group_rules"))

	testCases := map[string]struct {
		filter        e2ecortex.RuleFilter
		resultCheckFn func(assert.TestingT, []*ruler.RuleGroup)
	}{
		"Filter for Alert Rules": {
			filter: e2ecortex.RuleFilter{
				RuleType: "alert",
			},
			resultCheckFn: func(t assert.TestingT, ruleGroups []*ruler.RuleGroup) {
				assert.Len(t, ruleGroups, alertCount, "Expected %d rules but got %d", alertCount, len(ruleGroups))
			},
		},
		"Filter for Recording Rules": {
			filter: e2ecortex.RuleFilter{
				RuleType: "record",
			},
			resultCheckFn: func(t assert.TestingT, ruleGroups []*ruler.RuleGroup) {
				assert.Len(t, ruleGroups, numRulesGroups-alertCount, "Expected %d rules but got %d", numRulesGroups-alertCount, len(ruleGroups))
			},
		},
		"Filter by Namespace Name": {
			filter: e2ecortex.RuleFilter{
				Namespaces: []string{namespaceNames[2]},
			},
			resultCheckFn: func(t assert.TestingT, ruleGroups []*ruler.RuleGroup) {
				assert.Len(t, ruleGroups, namespaceNameCount[2], "Expected %d rules but got %d", namespaceNameCount[2], len(ruleGroups))
			},
		},
		"Filter by Namespace Name and Alert Rules": {
			filter: e2ecortex.RuleFilter{
				RuleType:   "alert",
				Namespaces: []string{namespaceNames[2]},
			},
			resultCheckFn: func(t assert.TestingT, ruleGroups []*ruler.RuleGroup) {
				for _, ruleGroup := range ruleGroups {
					rule := ruleGroup.Rules[0].(map[string]interface{})
					ruleType := rule["type"]
					assert.Equal(t, "alerting", ruleType, "Expected 'alerting' rule type but got %s", ruleType)
				}
			},
		},
		"Filter by Rule Names": {
			filter: e2ecortex.RuleFilter{
				RuleNames: []string{"rule_3", "rule_64", "rule_99"},
			},
			resultCheckFn: func(t assert.TestingT, ruleGroups []*ruler.RuleGroup) {
				ruleNames := []string{}
				for _, ruleGroup := range ruleGroups {
					rule := ruleGroup.Rules[0].(map[string]interface{})
					ruleName := rule["name"]
					ruleNames = append(ruleNames, ruleName.(string))

				}
				assert.Len(t, ruleNames, 3, "Expected %d rules but got %d", 3, len(ruleNames))
			},
		},
	}
	// For each test case, fetch the rules with configured filters, and ensure the results match.
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			actualGroups, err := c.GetPrometheusRules(tc.filter)
			require.NoError(t, err)
			tc.resultCheckFn(t, actualGroups)
		})
	}
}

func TestRulerAlertmanager(t *testing.T) {
	var namespaceOne = "test_/encoded_+namespace/?"
	ruleGroup := createTestRuleGroup(t)

	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	// Start dependencies.
	consul := e2edb.NewConsul()
	minio := e2edb.NewMinio(9000, rulestoreBucketName, bucketName)
	require.NoError(t, s.StartAndWaitReady(consul, minio))

	// Have at least one alertmanager configuration.
	require.NoError(t, writeFileToSharedDir(s, "alertmanager_configs/user-1.yaml", []byte(cortexAlertmanagerUserConfigYaml)))

	// Start Alertmanagers.
	amFlags := mergeFlags(AlertmanagerFlags(), AlertmanagerLocalFlags())
	am1 := e2ecortex.NewAlertmanager("alertmanager1", amFlags, "")
	am2 := e2ecortex.NewAlertmanager("alertmanager2", amFlags, "")
	require.NoError(t, s.StartAndWaitReady(am1, am2))

	am1URL := "http://" + am1.HTTPEndpoint()
	am2URL := "http://" + am2.HTTPEndpoint()

	// Connect the ruler to Alertmanagers
	configOverrides := map[string]string{
		"-ruler.alertmanager-url": strings.Join([]string{am1URL, am2URL}, ","),
	}

	// Start Ruler.
	ruler := e2ecortex.NewRuler("ruler", consul.NetworkHTTPEndpoint(), mergeFlags(BlocksStorageFlags(), RulerFlags(), configOverrides), "")
	require.NoError(t, s.StartAndWaitReady(ruler))

	// Create a client with the ruler address configured
	c, err := e2ecortex.NewClient("", "", "", ruler.HTTPEndpoint(), "user-1")
	require.NoError(t, err)

	// Set the rule group into the ruler
	require.NoError(t, c.SetRuleGroup(ruleGroup, namespaceOne))

	// Wait until the user manager is created
	require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(1), "cortex_ruler_managers_total"))

	//  Wait until we've discovered the alertmanagers.
	require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(2), []string{"cortex_prometheus_notifications_alertmanagers_discovered"}, e2e.WaitMissingMetrics))
}

func TestRulerAlertmanagerTLS(t *testing.T) {
	var namespaceOne = "test_/encoded_+namespace/?"
	ruleGroup := createTestRuleGroup(t)

	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	// Start dependencies.
	consul := e2edb.NewConsul()
	minio := e2edb.NewMinio(9000, rulestoreBucketName, bucketName)
	require.NoError(t, s.StartAndWaitReady(consul, minio))

	// set the ca
	cert := ca.New("Ruler/Alertmanager Test")

	// Ensure the entire path of directories exist.
	require.NoError(t, os.MkdirAll(filepath.Join(s.SharedDir(), "certs"), os.ModePerm))

	require.NoError(t, cert.WriteCACertificate(filepath.Join(s.SharedDir(), caCertFile)))

	// server certificate
	require.NoError(t, cert.WriteCertificate(
		&x509.Certificate{
			Subject:     pkix.Name{CommonName: "client"},
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		},
		filepath.Join(s.SharedDir(), clientCertFile),
		filepath.Join(s.SharedDir(), clientKeyFile),
	))
	require.NoError(t, cert.WriteCertificate(
		&x509.Certificate{
			Subject:     pkix.Name{CommonName: "server"},
			DNSNames:    []string{"ruler.alertmanager-client"},
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		},
		filepath.Join(s.SharedDir(), serverCertFile),
		filepath.Join(s.SharedDir(), serverKeyFile),
	))

	// Have at least one alertmanager configuration.
	require.NoError(t, writeFileToSharedDir(s, "alertmanager_configs/user-1.yaml", []byte(cortexAlertmanagerUserConfigYaml)))

	// Start Alertmanagers.
	amFlags := mergeFlags(
		AlertmanagerFlags(),
		AlertmanagerLocalFlags(),
		getServerHTTPTLSFlags(),
	)
	am1 := e2ecortex.NewAlertmanagerWithTLS("alertmanager1", amFlags, "")
	require.NoError(t, s.StartAndWaitReady(am1))

	// Connect the ruler to the Alertmanager
	configOverrides := mergeFlags(
		map[string]string{
			"-ruler.alertmanager-url": "https://" + am1.HTTPEndpoint(),
		},
		getTLSFlagsWithPrefix("ruler.alertmanager-client", "alertmanager", true),
	)

	// Start Ruler.
	ruler := e2ecortex.NewRuler("ruler", consul.NetworkHTTPEndpoint(), mergeFlags(BlocksStorageFlags(), RulerFlags(), configOverrides), "")
	require.NoError(t, s.StartAndWaitReady(ruler))

	// Create a client with the ruler address configured
	c, err := e2ecortex.NewClient("", "", "", ruler.HTTPEndpoint(), "user-1")
	require.NoError(t, err)

	// Set the rule group into the ruler
	require.NoError(t, c.SetRuleGroup(ruleGroup, namespaceOne))

	// Wait until the user manager is created
	require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(1), "cortex_ruler_managers_total"))

	//  Wait until we've discovered the alertmanagers.
	require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(1), []string{"cortex_prometheus_notifications_alertmanagers_discovered"}, e2e.WaitMissingMetrics))
}

func TestRulerMetricsForInvalidQueries(t *testing.T) {
	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	// Start dependencies.
	consul := e2edb.NewConsul()
	minio := e2edb.NewMinio(9000, bucketName, rulestoreBucketName)
	require.NoError(t, s.StartAndWaitReady(consul, minio))

	// Configure the ruler.
	flags := mergeFlags(
		BlocksStorageFlags(),
		RulerFlags(),
		map[string]string{
			// Since we're not going to run any rule (our only rule is invalid), we don't need the
			// store-gateway to be configured to a valid address.
			"-querier.store-gateway-addresses": "localhost:12345",
			// Enable the bucket index so we can skip the initial bucket scan.
			"-blocks-storage.bucket-store.bucket-index.enabled": "true",
			// Evaluate rules often, so that we don't need to wait for metrics to show up.
			"-ruler.evaluation-interval": "2s",
			"-ruler.poll-interval":       "2s",
			// No delay
			"-ruler.evaluation-delay-duration": "0",

			"-blocks-storage.tsdb.block-ranges-period":   "1h",
			"-blocks-storage.bucket-store.sync-interval": "1s",
			"-blocks-storage.tsdb.retention-period":      "2h",

			// We run single ingester only, no replication.
			"-distributor.replication-factor": "1",

			// Very low limit so that ruler hits it.
			"-querier.max-fetched-chunks-per-query": "5",
		},
	)

	const namespace = "test"
	const user = "user"

	distributor := e2ecortex.NewDistributor("distributor", e2ecortex.RingStoreConsul, consul.NetworkHTTPEndpoint(), flags, "")
	ruler := e2ecortex.NewRuler("ruler", consul.NetworkHTTPEndpoint(), flags, "")
	ingester := e2ecortex.NewIngester("ingester", e2ecortex.RingStoreConsul, consul.NetworkHTTPEndpoint(), flags, "")
	require.NoError(t, s.StartAndWaitReady(distributor, ingester, ruler))

	// Wait until both the distributor and ruler have updated the ring. The querier will also watch
	// the store-gateway ring if blocks sharding is enabled.
	require.NoError(t, distributor.WaitSumMetrics(e2e.Equals(512), "cortex_ring_tokens_total"))
	require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(512), "cortex_ring_tokens_total"))

	c, err := e2ecortex.NewClient(distributor.HTTPEndpoint(), "", "", ruler.HTTPEndpoint(), user)
	require.NoError(t, err)

	// Push some series to Cortex -- enough so that we can hit some limits.
	for i := 0; i < 10; i++ {
		series, _ := generateSeries("metric", time.Now(), prompb.Label{Name: "foo", Value: fmt.Sprintf("%d", i)})

		res, err := c.Push(series)
		require.NoError(t, err)
		require.Equal(t, 200, res.StatusCode)
	}

	matcher := labels.MustNewMatcher(labels.MatchEqual, "user", user)

	// Verify that user-failures don't increase cortex_ruler_queries_failed_total
	for groupName, expression := range map[string]string{
		// Syntactically correct expression (passes check in ruler), but failing because of invalid regex. This fails in PromQL engine.
		"invalid_group": `label_replace(metric, "foo", "$1", "service", "[")`,

		// This one fails in querier code, because of limits.
		"too_many_chunks_group": `sum(metric)`,
	} {
		t.Run(groupName, func(t *testing.T) {
			require.NoError(t, c.SetRuleGroup(ruleGroupWithRule(groupName, "rule", expression), namespace))
			m := ruleGroupMatcher(user, namespace, groupName)

			// Wait until ruler has loaded the group.
			require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(1), []string{"cortex_prometheus_rule_group_rules"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))

			// Wait until rule group has tried to evaluate the rule.
			require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.GreaterOrEqual(1), []string{"cortex_prometheus_rule_evaluations_total"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))

			// Verify that evaluation of the rule failed.
			require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.GreaterOrEqual(1), []string{"cortex_prometheus_rule_evaluation_failures_total"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))

			// But these failures were not reported as "failed queries"
			sum, err := ruler.SumMetrics([]string{"cortex_ruler_queries_failed_total"}, e2e.WithLabelMatchers(matcher))
			require.NoError(t, err)
			require.Equal(t, float64(0), sum[0])

			// Check that cortex_ruler_queries_total went up
			totalQueries, err := ruler.SumMetrics([]string{"cortex_ruler_queries_total"}, e2e.WithLabelMatchers(matcher))
			require.NoError(t, err)
			require.Greater(t, totalQueries[0], float64(0))

			// Delete rule before checkin "cortex_ruler_queries_total", as we want to reuse value for next test.
			require.NoError(t, c.DeleteRuleGroup(namespace, groupName))

			// Wait until ruler has unloaded the group. We don't use any matcher, so there should be no groups (in fact, metric disappears).
			require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(0), []string{"cortex_prometheus_rule_group_rules"}, e2e.SkipMissingMetrics))

			// Deleting the rule group should clean up the cortex_ruler_queries_total metrics
			_, err = ruler.SumMetrics([]string{"cortex_ruler_queries_total"}, e2e.WithLabelMatchers(matcher))
			require.EqualError(t, err, "metric=cortex_ruler_queries_total service=ruler: metric not found")
		})
	}

	// Now let's upload a non-failing rule, and make sure that it works.
	t.Run("real_error", func(t *testing.T) {
		const groupName = "good_rule"
		const expression = `sum(metric{foo=~"1|2"})`

		require.NoError(t, c.SetRuleGroup(ruleGroupWithRule(groupName, "rule", expression), namespace))
		m := ruleGroupMatcher(user, namespace, groupName)

		// Wait until ruler has loaded the group.
		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(1), []string{"cortex_prometheus_rule_group_rules"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))

		// Wait until rule group has tried to evaluate the rule, and succeeded.
		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.GreaterOrEqual(1), []string{"cortex_prometheus_rule_evaluations_total"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))
		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(0), []string{"cortex_prometheus_rule_evaluation_failures_total"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))

		// Still no failures.
		sum, err := ruler.SumMetrics([]string{"cortex_ruler_queries_failed_total"}, e2e.WithLabelMatchers(matcher))
		require.NoError(t, err)
		require.Equal(t, float64(0), sum[0])

		// Now let's stop ingester, and recheck metrics. This should increase cortex_ruler_queries_failed_total failures.
		require.NoError(t, s.Stop(ingester))

		// We should start getting "real" failures now.
		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.GreaterOrEqual(1), []string{"cortex_ruler_queries_failed_total"}, e2e.WithLabelMatchers(matcher)))
	})
}

func TestRulerMetricsWhenIngesterFails(t *testing.T) {
	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	// Start dependencies.
	consul := e2edb.NewConsul()
	minio := e2edb.NewMinio(9000, bucketName, rulestoreBucketName)
	require.NoError(t, s.StartAndWaitReady(consul, minio))

	const blockRangePeriod = 2 * time.Second
	// Configure the ruler.
	flags := mergeFlags(
		BlocksStorageFlags(),
		RulerFlags(),
		map[string]string{
			"-blocks-storage.tsdb.block-ranges-period":         blockRangePeriod.String(),
			"-blocks-storage.tsdb.ship-interval":               "1s",
			"-blocks-storage.bucket-store.sync-interval":       "1s",
			"-blocks-storage.bucket-store.index-cache.backend": tsdb.IndexCacheBackendInMemory,
			"-blocks-storage.tsdb.retention-period":            ((blockRangePeriod * 2) - 1).String(),

			// Enable the bucket index so we can skip the initial bucket scan.
			"-blocks-storage.bucket-store.bucket-index.enabled": "false",
			// Evaluate rules often, so that we don't need to wait for metrics to show up.
			"-ruler.evaluation-interval": "2s",
			"-ruler.poll-interval":       "2s",
			// No delay
			"-ruler.evaluation-delay-duration": "0",

			// We run single ingester only, no replication.
			"-distributor.replication-factor": "1",

			// Very low limit so that ruler hits it.
			"-querier.max-fetched-chunks-per-query": "15",
			"-querier.query-store-after":            (1 * time.Second).String(),
			"-querier.query-ingesters-within":       (2 * time.Second).String(),
		},
	)

	const namespace = "test"
	const user = "user"

	storeGateway := e2ecortex.NewStoreGateway("store-gateway-1", e2ecortex.RingStoreConsul, consul.NetworkHTTPEndpoint(), flags, "")

	flags = mergeFlags(flags, map[string]string{
		"-querier.store-gateway-addresses": storeGateway.NetworkGRPCEndpoint(),
	})

	distributor := e2ecortex.NewDistributor("distributor", e2ecortex.RingStoreConsul, consul.NetworkHTTPEndpoint(), flags, "")
	ruler := e2ecortex.NewRuler("ruler", consul.NetworkHTTPEndpoint(), flags, "")
	ingester := e2ecortex.NewIngester("ingester", e2ecortex.RingStoreConsul, consul.NetworkHTTPEndpoint(), flags, "")
	require.NoError(t, s.StartAndWaitReady(distributor, ingester, ruler, storeGateway))

	// Wait until both the distributor and ruler have updated the ring. The querier will also watch
	// the store-gateway ring if blocks sharding is enabled.
	require.NoError(t, distributor.WaitSumMetrics(e2e.Equals(512), "cortex_ring_tokens_total"))
	require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(1024), "cortex_ring_tokens_total"))

	c, err := e2ecortex.NewClient(distributor.HTTPEndpoint(), "", "", ruler.HTTPEndpoint(), user)
	require.NoError(t, err)

	matcher := labels.MustNewMatcher(labels.MatchEqual, "user", user)
	expression := "absent(sum_over_time(metric{}[2s] offset 1h))"

	// Now let's upload a non-failing rule, and make sure that it works.
	t.Run("real_error", func(t *testing.T) {
		const groupName = "good_rule"

		var ruleEvalCount float64
		ruleGroup := ruleGroupWithRule(groupName, "rule", expression)
		ruleGroup.Interval = 2
		require.NoError(t, c.SetRuleGroup(ruleGroup, namespace))
		m := ruleGroupMatcher(user, namespace, groupName)

		// Wait until ruler has loaded the group.
		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(1), []string{"cortex_prometheus_rule_group_rules"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))

		// Wait until rule group has tried to evaluate the rule, and succeeded.
		ruleEvalCount++
		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.GreaterOrEqual(ruleEvalCount), []string{"cortex_prometheus_rule_evaluations_total"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))
		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(0), []string{"cortex_prometheus_rule_evaluation_failures_total"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))

		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.GreaterOrEqual(1), []string{"cortex_ruler_write_requests_total"}, e2e.WithLabelMatchers(matcher), e2e.WaitMissingMetrics))
		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(0), []string{"cortex_ruler_write_requests_failed_total"}, e2e.WithLabelMatchers(matcher), e2e.WaitMissingMetrics))

		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(0), []string{"cortex_ruler_queries_failed_total"}, e2e.WithLabelMatchers(matcher), e2e.WaitMissingMetrics))

		// Wait until the TSDB head is compacted and shipped to the storage.
		// The shipped block contains the 1st series, while the 2ns series in the head.
		require.NoError(t, ingester.WaitSumMetrics(e2e.Equals(1), "cortex_ingester_shipper_uploads_total"))

		// Now let's stop ingester, and recheck metrics. This should increase cortex_ruler_write_requests_failed_total failures.
		require.NoError(t, s.Stop(ingester))
		ruleEvalCount++
		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.GreaterOrEqual(ruleEvalCount), []string{"cortex_prometheus_rule_evaluations_total"}, e2e.WithLabelMatchers(m), e2e.WaitMissingMetrics))

		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(0), []string{"cortex_ruler_queries_failed_total"}, e2e.WithLabelMatchers(matcher), e2e.WaitMissingMetrics))
		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.GreaterOrEqual(2), []string{"cortex_ruler_write_requests_total"}, e2e.WithLabelMatchers(matcher), e2e.WaitMissingMetrics))

		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.GreaterOrEqual(1), []string{"cortex_ruler_write_requests_failed_total"}, e2e.WithLabelMatchers(matcher), e2e.WaitMissingMetrics))
	})
}

func TestRulerDisablesRuleGroups(t *testing.T) {
	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	// Start dependencies.
	consul := e2edb.NewConsul()
	minio := e2edb.NewMinio(9000, bucketName, rulestoreBucketName)
	require.NoError(t, s.StartAndWaitReady(consul, minio))

	const blockRangePeriod = 2 * time.Second
	// Configure the ruler.
	flags := mergeFlags(
		BlocksStorageFlags(),
		RulerFlags(),
		map[string]string{
			"-blocks-storage.tsdb.block-ranges-period":         blockRangePeriod.String(),
			"-blocks-storage.tsdb.ship-interval":               "1s",
			"-blocks-storage.bucket-store.sync-interval":       "1s",
			"-blocks-storage.bucket-store.index-cache.backend": tsdb.IndexCacheBackendInMemory,
			"-blocks-storage.tsdb.retention-period":            ((blockRangePeriod * 2) - 1).String(),

			// Enable the bucket index so we can skip the initial bucket scan.
			"-blocks-storage.bucket-store.bucket-index.enabled": "false",
			// Evaluate rules often, so that we don't need to wait for metrics to show up.
			"-ruler.evaluation-interval": "2s",
			"-ruler.poll-interval":       "2s",
			// No delay
			"-ruler.evaluation-delay-duration": "0",

			// We run single ingester only, no replication.
			"-distributor.replication-factor": "1",

			// Very low limit so that ruler hits it.
			"-querier.max-fetched-chunks-per-query": "15",
			"-querier.query-store-after":            (1 * time.Second).String(),
			"-querier.query-ingesters-within":       (2 * time.Second).String(),
		},
	)

	const namespace = "test"
	const user = "user"
	configFileName := "runtime-config.yaml"
	bucketName := "cortex"

	storeGateway := e2ecortex.NewStoreGateway("store-gateway-1", e2ecortex.RingStoreConsul, consul.NetworkHTTPEndpoint(), flags, "")

	flags = mergeFlags(flags, map[string]string{
		"-querier.store-gateway-addresses":     storeGateway.NetworkGRPCEndpoint(),
		"-runtime-config.backend":              "s3",
		"-runtime-config.s3.access-key-id":     e2edb.MinioAccessKey,
		"-runtime-config.s3.secret-access-key": e2edb.MinioSecretKey,
		"-runtime-config.s3.bucket-name":       bucketName,
		"-runtime-config.s3.endpoint":          fmt.Sprintf("%s-minio-9000:9000", networkName),
		"-runtime-config.s3.insecure":          "true",
		"-runtime-config.file":                 configFileName,
		"-runtime-config.reload-period":        "2s",
	})

	distributor := e2ecortex.NewDistributor("distributor", e2ecortex.RingStoreConsul, consul.NetworkHTTPEndpoint(), flags, "")

	client, err := s3.NewBucketWithConfig(nil, s3.Config{
		Endpoint:  minio.HTTPEndpoint(),
		Insecure:  true,
		Bucket:    bucketName,
		AccessKey: e2edb.MinioAccessKey,
		SecretKey: e2edb.MinioSecretKey,
	}, "runtime-config-test")

	require.NoError(t, err)

	// update runtime config
	newRuntimeConfig := []byte(`overrides:
  user:
    disabled_rule_groups:
      - name: bad_rule
        namespace: test`)
	require.NoError(t, client.Upload(context.Background(), configFileName, bytes.NewReader(newRuntimeConfig)))
	time.Sleep(2 * time.Second)

	ruler := e2ecortex.NewRuler("ruler", consul.NetworkHTTPEndpoint(), flags, "")

	ingester := e2ecortex.NewIngester("ingester", e2ecortex.RingStoreConsul, consul.NetworkHTTPEndpoint(), flags, "")
	require.NoError(t, s.StartAndWaitReady(distributor, ingester, ruler, storeGateway))

	// Wait until both the distributor and ruler have updated the ring. The querier will also watch
	// the store-gateway ring if blocks sharding is enabled.
	require.NoError(t, distributor.WaitSumMetrics(e2e.Equals(512), "cortex_ring_tokens_total"))
	require.NoError(t, ruler.WaitSumMetrics(e2e.Equals(1024), "cortex_ring_tokens_total"))

	c, err := e2ecortex.NewClient(distributor.HTTPEndpoint(), "", "", ruler.HTTPEndpoint(), user)
	require.NoError(t, err)

	expression := "absent(sum_over_time(metric{}[2s] offset 1h))"

	t.Run("disable_rule_group", func(t *testing.T) {

		ruleGroup := ruleGroupWithRule("bad_rule", "rule", expression)
		ruleGroup.Interval = 2
		require.NoError(t, c.SetRuleGroup(ruleGroup, namespace))

		ruleGroup = ruleGroupWithRule("good_rule", "rule", expression)
		ruleGroup.Interval = 2
		require.NoError(t, c.SetRuleGroup(ruleGroup, namespace))

		m1 := ruleGroupMatcher(user, namespace, "good_rule")

		// Wait until ruler has loaded the group.
		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.GreaterOrEqual(1), []string{"cortex_ruler_sync_rules_total"}, e2e.WaitMissingMetrics))

		require.NoError(t, ruler.WaitSumMetricsWithOptions(e2e.Equals(1), []string{"cortex_prometheus_rule_group_rules"}, e2e.WithLabelMatchers(m1), e2e.WaitMissingMetrics))

		filter := e2ecortex.RuleFilter{}
		actualGroups, err := c.GetPrometheusRules(filter)
		require.NoError(t, err)
		assert.Equal(t, 1, len(actualGroups))
		assert.Equal(t, "good_rule", actualGroups[0].Name)
		assert.Equal(t, "test", actualGroups[0].File)
	})
}

func ruleGroupMatcher(user, namespace, groupName string) *labels.Matcher {
	return labels.MustNewMatcher(labels.MatchEqual, "rule_group", fmt.Sprintf("/rules/%s/%s;%s", user, namespace, groupName))
}

func ruleGroupWithRule(groupName string, ruleName string, expression string) rulefmt.RuleGroup {
	// Prepare rule group with invalid rule.
	var recordNode = yaml.Node{}
	var exprNode = yaml.Node{}

	recordNode.SetString(ruleName)
	exprNode.SetString(expression)

	return rulefmt.RuleGroup{
		Name:     groupName,
		Interval: 10,
		Rules: []rulefmt.RuleNode{{
			Record: recordNode,
			Expr:   exprNode,
		}},
	}
}

func createTestRuleGroup(t *testing.T) rulefmt.RuleGroup {
	t.Helper()

	var (
		recordNode = yaml.Node{}
		exprNode   = yaml.Node{}
	)

	recordNode.SetString("test_rule")
	exprNode.SetString("up")
	return rulefmt.RuleGroup{
		Name:     "test_encoded_+\"+group_name/?",
		Interval: 100,
		Rules: []rulefmt.RuleNode{
			{
				Record: recordNode,
				Expr:   exprNode,
			},
		},
	}
}

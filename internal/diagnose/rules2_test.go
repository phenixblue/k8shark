package diagnose

import "testing"

func phase2Fixture() map[string]string {
	// One Running pod with no requests/limits (healthy otherwise).
	pods := `{"kind":"PodList","apiVersion":"v1","items":[
	  {"metadata":{"name":"app-1"},"spec":{"containers":[{"name":"c"}]},"status":{"phase":"Running","containerStatuses":[{"name":"c","ready":true,"restartCount":0,"state":{"running":{}}}]}}
	]}`
	deploys := `{"kind":"DeploymentList","apiVersion":"apps/v1","items":[
	  {"metadata":{"name":"web"},"spec":{"replicas":3},"status":{"readyReplicas":1}}
	]}`
	nodes := `{"kind":"NodeList","apiVersion":"v1","items":[
	  {"metadata":{"name":"n1"},"status":{"conditions":[{"type":"Ready","status":"False"},{"type":"DiskPressure","status":"True"}]}}
	]}`
	ingresses := `{"kind":"IngressList","apiVersion":"extensions/v1beta1","items":[]}`
	return map[string]string{
		"/api/v1/namespaces/ops/pods":                       pods,
		"/apis/apps/v1/namespaces/ops/deployments":          deploys,
		"/api/v1/nodes":                                     nodes,
		"/apis/extensions/v1beta1/namespaces/ops/ingresses": ingresses,
	}
}

func TestRun_Phase2Rules(t *testing.T) {
	store := buildDiagStore(t, phase2Fixture())
	rep := Run(store, Options{})
	by := ruleIDs(rep.Findings)

	checks := map[string]string{
		"workload.no-requests":          SeverityWarning,
		"workload.no-limits":            SeverityInfo,
		"workload.replicas-unavailable": SeverityWarning,
		"node.not-ready":                SeverityCritical,
		"node.pressure":                 SeverityWarning,
		"cluster.deprecated-api":        SeverityWarning,
	}
	for rule, sev := range checks {
		f, ok := by[rule]
		if !ok {
			t.Errorf("missing expected finding %q; got %v", rule, keys(by))
			continue
		}
		if f.Severity != sev {
			t.Errorf("%s severity = %q, want %q", rule, f.Severity, sev)
		}
	}
	if f := by["workload.replicas-unavailable"]; f.Evidence != "1/3 replicas ready" {
		t.Errorf("replicas evidence = %q", f.Evidence)
	}
	if f := by["cluster.deprecated-api"]; f.Object.APIPath == "" {
		t.Errorf("deprecated-api finding missing api_path: %+v", f)
	}
}

func TestDeprecatedAPI_IndexOnly(t *testing.T) {
	// The deprecated-api rule is index-driven; an empty item list still flags.
	store := buildDiagStore(t, map[string]string{
		"/apis/batch/v1beta1/namespaces/x/cronjobs": `{"kind":"CronJobList","items":[]}`,
		"/api/v1/namespaces/x/pods":                 `{"kind":"PodList","items":[]}`, // core/v1 not flagged
	})
	rep := Run(store, Options{Category: "cluster"})
	if len(rep.Findings) != 1 || rep.Findings[0].RuleID != "cluster.deprecated-api" {
		t.Fatalf("expected one deprecated-api finding, got %v", keys(ruleIDs(rep.Findings)))
	}
}

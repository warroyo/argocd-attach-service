package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestMatchTargets(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		target   string
		want     bool
	}{
		{"exact match", []string{"team-a"}, "team-a", true},
		{"exact mismatch", []string{"team-a"}, "team-b", false},
		{"glob prefix match", []string{"apps-*"}, "apps-frontend", true},
		{"glob prefix mismatch", []string{"apps-*"}, "team-a", false},
		{"wildcard matches anything", []string{"*"}, "anything-at-all", true},
		{"empty pattern never matches", []string{""}, "", false},
		{"empty pattern skipped among others", []string{"", "team-a"}, "team-a", true},
		{"no patterns never matches", []string{}, "team-a", false},
		{"nil patterns never matches", nil, "team-a", false},
		{"second pattern matches", []string{"team-a", "team-b"}, "team-b", true},
		{"glob does not cross characters it should not", []string{"team-?"}, "team-ab", false},
		{"invalid glob falls through without matching", []string{"[unclosed"}, "unclosed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchTargets(tc.patterns, tc.target); got != tc.want {
				t.Errorf("matchTargets(%v, %q) = %v, want %v", tc.patterns, tc.target, got, tc.want)
			}
		})
	}
}

func TestCheckDenylistFloor(t *testing.T) {
	origControllerNamespace := controllerNamespace
	defer func() { controllerNamespace = origControllerNamespace }()

	cases := []struct {
		name         string
		target       string
		ownNs        string
		controllerNs string
		designated   []string
		blocked      []string
		wantErr      bool
	}{
		{"plain namespace allowed", "team-a", "argocd", "", nil, nil, false},
		{"empty target rejected", "", "argocd", "", nil, nil, true},
		{"kube-system rejected", "kube-system", "argocd", "", nil, nil, true},
		{"kube-public rejected", "kube-public", "argocd", "", nil, nil, true},
		{"kube-node-lease rejected", "kube-node-lease", "argocd", "", nil, nil, true},
		{"vmware-system prefix rejected", "vmware-system-netop", "argocd", "", nil, nil, true},
		{"svc prefix rejected", "svc-argocd-operator", "argocd", "", nil, nil, true},
		{"own namespace rejected", "argocd", "argocd", "", nil, nil, true},
		{"controller namespace rejected when set", "controller-ns", "argocd", "controller-ns", nil, nil, true},
		{"controller namespace ignored when unset", "controller-ns", "argocd", "", nil, nil, false},
		{"designated namespace rejected", "argocd-two", "argocd", "", []string{"argocd", "argocd-two"}, nil, true},
		{"blocked namespace rejected", "no-attach", "argocd", "", nil, []string{"no-attach"}, true},
		{"prefix must anchor at start", "my-vmware-system-thing", "argocd", "", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			controllerNamespace = tc.controllerNs
			err := checkDenylistFloor(tc.target, tc.ownNs, tc.designated, tc.blocked)
			if (err != nil) != tc.wantErr {
				t.Errorf("checkDenylistFloor(%q, %q, %v, %v) error = %v, wantErr %v", tc.target, tc.ownNs, tc.designated, tc.blocked, err, tc.wantErr)
			}
		})
	}
}

func TestRemoteSAName(t *testing.T) {
	t.Run("normal name", func(t *testing.T) {
		if got := remoteSAName("argocd"); got != "argo-attach-argocd" {
			t.Errorf("remoteSAName(argocd) = %q, want argo-attach-argocd", got)
		}
	})

	longNs := strings.Repeat("a", 80)

	t.Run("long name fits token suffix within dns limit", func(t *testing.T) {
		got := remoteSAName(longNs)
		if len(got) > 57 {
			t.Errorf("remoteSAName length = %d, want <= 57 so %q-token fits in 63", len(got), got)
		}
		if len(got+"-token") > 63 {
			t.Errorf("token secret name %q-token exceeds 63 chars", got)
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		if remoteSAName(longNs) != remoteSAName(longNs) {
			t.Error("remoteSAName is not deterministic for the same input")
		}
	})

	t.Run("distinct long inputs give distinct names", func(t *testing.T) {
		other := strings.Repeat("a", 79) + "b"
		if remoteSAName(longNs) == remoteSAName(other) {
			t.Error("distinct namespaces produced colliding SA names")
		}
	})
}

func TestUpdateGenericStatus(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		status := updateGenericStatus(nil, false, fmt.Errorf("boom"))
		if status["state"] != "Failed" || status["ready"] != false {
			t.Errorf("unexpected failure status: %v", status)
		}
		if msg, _ := status["message"].(string); !strings.Contains(msg, "boom") {
			t.Errorf("failure message %q does not contain the reconcile error", msg)
		}
	})

	t.Run("success", func(t *testing.T) {
		status := updateGenericStatus(nil, true, nil)
		if status["state"] != "Ready" || status["ready"] != true {
			t.Errorf("unexpected success status: %v", status)
		}
	})

	t.Run("pending", func(t *testing.T) {
		status := updateGenericStatus(nil, false, nil)
		if status["state"] != "Pending" || status["ready"] != false {
			t.Errorf("unexpected pending status: %v", status)
		}
	})
}

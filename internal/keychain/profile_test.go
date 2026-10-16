package keychain_test

import (
	"errors"
	"testing"

	"github.com/patrikmichi/zuano/internal/keychain"
)

func TestProfilesStoreSeparateSessionsForOneAccount(t *testing.T) {
	const email = "profiles@example.com"
	human := keychain.TokenData{AccessToken: "human-access", RefreshToken: "human-refresh"}
	agent := keychain.TokenData{AccessToken: "agent-access", RefreshToken: "agent-refresh"}
	if err := keychain.WriteToken(testOrigin, email, human); err != nil {
		t.Fatal(err)
	}
	if err := keychain.WriteProfileToken(keychain.ProfileAgent, testOrigin, email, agent); err != nil {
		t.Fatal(err)
	}

	gotHuman, err := keychain.ReadToken(testOrigin, email)
	if err != nil || gotHuman.AccessToken != "human-access" {
		t.Fatalf("human session = %+v, %v", gotHuman, err)
	}
	gotAgent, err := keychain.ReadProfileToken(keychain.ProfileAgent, testOrigin, email)
	if err != nil || gotAgent.AccessToken != "agent-access" {
		t.Fatalf("agent session = %+v, %v", gotAgent, err)
	}

	if err := keychain.DeleteProfileToken(keychain.ProfileAgent, testOrigin, email); err != nil {
		t.Fatal(err)
	}
	if _, err := keychain.ReadProfileToken(keychain.ProfileAgent, testOrigin, email); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("agent session after delete: %v, want not found", err)
	}
	if _, err := keychain.ReadToken(testOrigin, email); err != nil {
		t.Errorf("deleting the agent session removed the human one: %v", err)
	}
}

func TestAgentProfileNeverReadsTheHumanSession(t *testing.T) {
	const email = "human-only@example.com"
	if err := keychain.WriteToken(testOrigin, email, keychain.TokenData{AccessToken: "human-access"}); err != nil {
		t.Fatal(err)
	}
	if _, err := keychain.ReadProfileToken(keychain.ProfileAgent, testOrigin, email); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("agent read of a human-only account: %v, want not found", err)
	}
}

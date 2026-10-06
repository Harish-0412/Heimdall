// control is an operator-only bootstrap utility. Migration credentials never
// enter the API or worker process. Bearer outputs go to an exclusive 0600 file.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/controlapi"
	"github.com/heimdall-dev/heimdall/internal/controlruntime"
	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/heimdall-dev/heimdall/internal/store"
)

type input struct {
	Tenant       domain.Tenant          `json:"tenant"`
	Policy       config.Policy          `json:"policy,omitempty"`
	Quota        domain.Quota           `json:"quota,omitempty"`
	Actor        string                 `json:"actor"`
	Credential   domain.CredentialInput `json:"credential,omitempty"`
	Installation domain.Installation    `json:"installation,omitempty"`
	Repository   domain.Repository      `json:"repository,omitempty"`
	Attestation  domain.DataAttestation `json:"attestation,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "control:", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: control migrate|provision|credential|repository|attest --input file --output private.json")
	}
	command := os.Args[1]
	if command != "migrate" && command != "provision" && command != "credential" && command != "repository" && command != "attest" {
		return errors.New("unknown operator command")
	}
	fs := flag.NewFlagSet("control", flag.ContinueOnError)
	inPath := fs.String("input", "", "operator JSON input")
	outPath := fs.String("output", "", "exclusive private JSON output")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if command == "migrate" {
		dsn := os.Getenv("HEIMDALL_MIGRATION_DATABASE_URL")
		if dsn == "" {
			return errors.New("HEIMDALL_MIGRATION_DATABASE_URL is required")
		}
		if store.Migrate(ctx, dsn) != nil {
			return errors.New("migration failed; inspect the database's private diagnostics")
		}
		fmt.Println("Migrations applied")
		return nil
	}
	b, err := os.ReadFile(*inPath)
	if err != nil || len(b) > 1<<20 {
		return errors.New("operator input is unreadable or oversized")
	}
	var in input
	if controlapi.StrictJSON(b, &in) != nil {
		return errors.New("operator input must match the documented JSON schema")
	}
	if _, err := uuid.Parse(in.Tenant.ID); err != nil {
		return errors.New("tenant.id must be a UUID")
	}
	if in.Actor == "" {
		return errors.New("operator actor is required")
	}
	// Reserve the output before mutating anything, so issued credentials have
	// a private destination. Existing files are never silently overwritten.
	f, err := os.OpenFile(*outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("output must be a new private file")
	}
	defer f.Close()
	if command == "provision" {
		if controlapi.ValidatePolicy(in.Policy) != nil {
			return errors.New("an explicit bounded tenant policy is required")
		}
		policy, _ := json.Marshal(in.Policy)
		dsn := os.Getenv("HEIMDALL_MIGRATION_DATABASE_URL")
		if dsn == "" {
			return errors.New("separate migration DSN is required")
		}
		if store.ProvisionTenant(ctx, dsn, in.Tenant, policy, in.Quota) != nil {
			return errors.New("tenant provisioning failed")
		}
	}
	s, err := controlruntime.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	p := domain.Principal{TenantID: in.Tenant.ID, ActorID: in.Actor, Role: "admin", Kind: "operator"}
	var result any
	switch command {
	case "provision":
		result, err = s.IssueCredential(ctx, p, domain.CredentialInput{ActorID: in.Actor, Role: "admin", Kind: "user", TTL: 24 * time.Hour})
	case "credential":
		result, err = s.IssueCredential(ctx, p, in.Credential)
	case "repository":
		if err = s.PutInstallation(ctx, p, in.Installation); err == nil {
			result, err = s.PutRepository(ctx, p, in.Repository)
		}
	case "attest":
		err = s.PutDataAttestation(ctx, p, in.Attestation)
		result = map[string]string{"result": "sanitised-data attestation recorded"}
	default:
		return errors.New("unknown operator command")
	}
	if err != nil {
		return errors.New("operator action failed; verify tenant scope, identifiers, and prerequisites")
	}
	if err = json.NewEncoder(f).Encode(result); err != nil {
		return errors.New("cannot persist private output")
	}
	if err = f.Sync(); err != nil {
		return errors.New("cannot persist private output")
	}
	fmt.Println("Operator result saved to", *outPath)
	return nil
}

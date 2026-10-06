package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/heimdall-dev/heimdall/internal/githubapp"
)

// DispatchOutbox delivers only committed state. The row lock prevents an old
// HTTP response arriving after a newer generation's comment/check update.
func (s *Service) DispatchOutbox(ctx context.Context) error {
	tenants, err := s.o.Store.ActiveTenants(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, tenant := range tenants {
		p := principal(tenant, "github:outbox")
		// Claim immediately before processing. Leasing a large batch and then
		// doing serial network calls can let the later leases expire unseen.
		items, err := s.o.Store.ClaimOutbox(ctx, p, 1)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, item := range items {
			job, cancel := context.WithTimeout(ctx, s.o.DeliveryTimeout)
			err = s.deliver(job, p, item)
			cancel()
			if err != nil {
				delay := time.Second * time.Duration(1<<min(item.Attempts, 8))
				j, _ := rand.Int(rand.Reader, big.NewInt(500))
				delay += time.Duration(j.Int64()) * time.Millisecond
				var apiErr *githubapp.APIError
				if errors.As(err, &apiErr) && apiErr.RetryAfter > delay {
					delay = apiErr.RetryAfter
				}
				if delay > time.Hour {
					delay = time.Hour
				}
				if retryErr := s.o.Store.RetryOutbox(ctx, p, item.ID, item.LeaseToken, delay, "GitHub delivery failed"); retryErr != nil {
					failures = append(failures, retryErr)
				}
			}
		}
	}
	return errors.Join(failures...)
}

func (s *Service) recoverComment(ctx context.Context, repo domain.Repository, number int64, marker string, id int64) (int64, error) {
	bot, err := s.o.GitHub.BotLogin(ctx)
	if err != nil {
		return 0, err
	}
	if id > 0 {
		comment, err := s.o.GitHub.Comment(ctx, ref(repo), id)
		if err == nil && comment.User.Login == bot && strings.Contains(comment.Body, marker) {
			return id, nil
		}
		if err != nil && !errors.Is(err, githubapp.ErrNotFound) {
			return 0, err
		}
	}
	comments, err := s.o.GitHub.ListComments(ctx, ref(repo), number)
	if err != nil {
		return 0, err
	}
	for _, c := range comments {
		if c.User.Login == bot && strings.Contains(c.Body, marker) {
			return c.ID, nil
		}
	}
	return 0, nil
}

func (s *Service) deliver(ctx context.Context, p domain.Principal, item domain.Outbox) error {
	if item.Kind == "github.notice" || item.Kind == "github.command" {
		var payload struct {
			RepositoryID string `json:"repositoryID"`
			PullRequest  int64  `json:"pullRequest"`
			DeliveryID   string `json:"deliveryID"`
			Body         string `json:"body"`
		}
		if json.Unmarshal(item.Payload, &payload) != nil || payload.RepositoryID == "" || payload.PullRequest < 1 || len(payload.Body) > 16384 {
			return errors.New("invalid GitHub message outbox")
		}
		if item.Kind == "github.notice" {
			err := s.o.Store.WithGitHubNotice(ctx, p, payload.RepositoryID, payload.PullRequest, func(repo domain.Repository, pr domain.PullRequest, e *domain.Environment) error {
				body := "Heimdall Preview: waiting for a trusted CI build."
				switch {
				case pr.Fork:
					body = "Heimdall Preview: fork pull requests are disabled."
				case pr.State == "refused":
					body = "Heimdall Preview: the current configuration was refused. Review the tenant policy and default-branch baseline."
				case pr.NeedsApproval && pr.ApprovedSHA != pr.HeadSHA:
					body = "Heimdall Preview: a maintainer must review this exact commit and comment `/heimdall approve " + pr.HeadSHA + "`."
				case e != nil:
					body, _ = Presentation(repo, *e)
				case pr.State == "closed":
					body = "Heimdall Preview: this pull request is closed."
				}
				marker := noticeMarker(repo, payload.PullRequest)
				id, err := s.recoverComment(ctx, repo, payload.PullRequest, marker, 0)
				if err != nil {
					return err
				}
				_, err = s.o.GitHub.WriteComment(ctx, ref(repo), payload.PullRequest, id, marker+"\n\n"+body)
				return err
			})
			if err != nil {
				return err
			}
			return s.o.Store.FinishOutbox(ctx, p, item.ID, item.LeaseToken, item.Generation, domain.GitHubDelivery{})
		}
		repo, err := s.o.Store.GetRepository(ctx, p, payload.RepositoryID)
		if err != nil {
			return err
		}
		marker := noticeMarker(repo, payload.PullRequest)
		if item.Kind == "github.command" {
			marker = "<!-- heimdall-command:v1:" + payload.DeliveryID + " -->"
		}
		id, err := s.recoverComment(ctx, repo, payload.PullRequest, marker, 0)
		if err != nil {
			return err
		}
		if _, err = s.o.GitHub.WriteComment(ctx, ref(repo), payload.PullRequest, id, marker+"\n\nHeimdall: "+payload.Body); err != nil {
			return err
		}
		return s.o.Store.FinishOutbox(ctx, p, item.ID, item.LeaseToken, item.Generation, domain.GitHubDelivery{})
	}
	if item.Kind != "github.preview" {
		return errors.New("unsupported outbox kind")
	}
	var delivered domain.GitHubDelivery
	err := s.o.Store.WithGitHubDelivery(ctx, p, item.EnvironmentID, func(e domain.Environment, d domain.GitHubDelivery) (domain.GitHubDelivery, error) {
		// Stale rows are discarded before any HTTP write. A later row will
		// deliver current state, including runtime updates in this generation.
		if e.Generation != item.Generation {
			return d, domain.ErrStaleGeneration
		}
		repo, err := s.o.Store.GetRepository(ctx, p, e.RepositoryID)
		if err != nil {
			return d, err
		}
		marker := noticeMarker(repo, e.PullRequest)
		commentID, err := s.recoverComment(ctx, repo, e.PullRequest, marker, d.CommentID)
		if err != nil {
			return d, err
		}
		body, check := Presentation(repo, e)
		commentID, err = s.o.GitHub.WriteComment(ctx, ref(repo), e.PullRequest, commentID, marker+"\n\n"+body)
		if err != nil {
			return d, err
		}
		// Each deployment has a check on its own commit. Never PATCH a prior
		// commit's check with a new generation's result.
		checkID := int64(0)
		checks, err := s.o.GitHub.Checks(ctx, ref(repo), e.Commit)
		if err != nil {
			return d, err
		}
		for _, existing := range checks {
			if existing.ExternalID == check.ExternalID && s.o.GitHub.OwnCheck(existing) {
				checkID = existing.ID
				break
			}
		}
		checkID, err = s.o.GitHub.WriteCheck(ctx, ref(repo), checkID, check)
		if err != nil {
			return d, err
		}
		delivered = domain.GitHubDelivery{EnvironmentID: e.ID, Generation: e.Generation, CommentID: commentID, CheckID: checkID}
		return delivered, nil
	})
	if errors.Is(err, domain.ErrStaleGeneration) {
		return s.o.Store.FinishOutbox(ctx, p, item.ID, item.LeaseToken, item.Generation, domain.GitHubDelivery{})
	}
	if err != nil {
		return err
	}
	return s.o.Store.FinishOutbox(ctx, p, item.ID, item.LeaseToken, item.Generation, delivered)
}

// Presentation deliberately exposes bounded summaries, never logs, raw
// configuration, commands, secrets or stack traces.
func Presentation(repo domain.Repository, e domain.Environment) (string, githubapp.CheckUpdate) {
	state := e.Phase
	if e.BuildState == "pending" {
		state = "Waiting for trusted CI build"
	}
	if e.BuildState == "refused" {
		state = "Preview refused"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### Heimdall Preview\n\n%s · generation %d · commit `%s`\n\n", state, e.Generation, e.Commit)
	if e.DesiredState == "Destroyed" {
		b.WriteString("Preview cleanup is complete when the status is Destroyed.\n\n")
	}
	var status struct {
		URLs []struct {
			Service string `json:"service"`
			URL     string `json:"url"`
		} `json:"urls"`
		Diagnoses []struct{ Code, Summary, Suggestion string } `json:"diagnoses"`
	}
	if json.Unmarshal(e.Status, &status) == nil && e.Phase == "Ready" {
		slices.SortFunc(status.URLs, func(a, b struct {
			Service string `json:"service"`
			URL     string `json:"url"`
		}) int {
			return strings.Compare(a.Service, b.Service)
		})
		for _, entry := range status.URLs {
			u, err := url.Parse(entry.URL)
			if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.User == nil && u.Host != "" && !strings.ContainsAny(entry.Service, "[]\n\r") {
				fmt.Fprintf(&b, "- %s: <%s>\n", entry.Service, u.String())
			}
		}
	}
	for i, d := range status.Diagnoses {
		if i >= 5 {
			break
		}
		if safePublicCode(d.Code) {
			fmt.Fprintf(&b, "- Diagnosis: `%s`. See the environment timeline for its redacted details.\n", d.Code)
		}
	}
	fmt.Fprintf(&b, "\nExpires: %s\n\nCommands for maintainers: `/heimdall reset`, `/heimdall retry`, `/heimdall extend 2h`, `/heimdall delete`.\n", e.ExpiresAt.UTC().Format(time.RFC3339))
	u := githubapp.CheckUpdate{Name: "Heimdall Preview", HeadSHA: e.Commit, ExternalID: checkExternal(e), Status: "in_progress"}
	u.Output.Title = "Heimdall Preview: " + state
	u.Output.Summary = b.String()
	switch {
	case e.Phase == "Ready" && e.BuildState == "ready":
		u.Status = "completed"
		u.Conclusion = "success"
	case e.Phase == "Failed" || e.BuildState == "refused":
		u.Status = "completed"
		u.Conclusion = "failure"
	case e.Phase == "Destroyed":
		u.Status = "completed"
		u.Conclusion = "neutral"
	}
	return b.String(), u
}
func safePublicCode(s string) bool {
	if len(s) > 64 || s == "" {
		return false
	}
	for _, c := range s {
		allowed := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.'
		if !allowed {
			return false
		}
	}
	return true
}

package githubapp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Repository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}
type PullRequest struct {
	Number    int64     `json:"number"`
	State     string    `json:"state"`
	Merged    bool      `json:"merged"`
	UpdatedAt time.Time `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		SHA  string      `json:"sha"`
		Repo *Repository `json:"repo"`
	} `json:"head"`
	Base struct {
		SHA  string      `json:"sha"`
		Ref  string      `json:"ref"`
		Repo *Repository `json:"repo"`
	} `json:"base"`
}

func (p PullRequest) Fork(repositoryID int64) bool {
	return p.Head.Repo == nil || p.Head.Repo.ID != repositoryID || p.Base.Repo == nil || p.Base.Repo.ID != repositoryID
}

type IssueComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}
type WorkflowRun struct {
	ID             int64      `json:"id"`
	RunAttempt     int64      `json:"run_attempt"`
	HeadSHA        string     `json:"head_sha"`
	Event          string     `json:"event"`
	Status         string     `json:"status"`
	Conclusion     string     `json:"conclusion"`
	Repository     Repository `json:"repository"`
	HeadRepository Repository `json:"head_repository"`
	PullRequests   []struct {
		Number int64 `json:"number"`
		Head   struct {
			SHA  string `json:"sha"`
			Repo struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"head"`
	} `json:"pull_requests"`
}

func (c *Client) Repository(ctx context.Context, r RepoRef) (Repository, error) {
	var out Repository
	err := c.do(ctx, r, map[string]string{"metadata": "read"}, "GET", strings.TrimSuffix(repoPath(r), "/"), nil, &out)
	if err == nil && out.ID != r.RepositoryID {
		err = errors.New("GitHub repository identity changed")
	}
	return out, err
}
func (c *Client) PullRequest(ctx context.Context, r RepoRef, n int64) (PullRequest, error) {
	var out PullRequest
	err := c.do(ctx, r, map[string]string{"pull_requests": "read"}, "GET", repoPath(r)+"pulls/"+strconv.FormatInt(n, 10), nil, &out)
	if err == nil && (out.Number != n || out.Base.Repo == nil || out.Base.Repo.ID != r.RepositoryID) {
		err = errors.New("GitHub pull request identity mismatch")
	}
	return out, err
}
func (c *Client) DefaultBranchSHA(ctx context.Context, r RepoRef, branch string) (string, error) {
	if branch == "" {
		return "", errors.New("repository has no default branch")
	}
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := c.do(ctx, r, map[string]string{"contents": "read"}, "GET", repoPath(r)+"git/ref/heads/"+url.PathEscape(branch), nil, &out)
	if err == nil && !ValidSHA(out.Object.SHA) {
		err = errors.New("GitHub returned invalid branch SHA")
	}
	return out.Object.SHA, err
}
func (c *Client) Config(ctx context.Context, r RepoRef, sha string) ([]byte, error) {
	if !ValidSHA(sha) {
		return nil, errors.New("config must be read at an immutable commit SHA")
	}
	var out struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		Size     int64  `json:"size"`
	}
	err := c.do(ctx, r, map[string]string{"contents": "read"}, "GET", repoPath(r)+"contents/heimdall.yaml?ref="+url.QueryEscape(sha), nil, &out)
	if err != nil {
		return nil, err
	}
	if out.Type != "file" || out.Encoding != "base64" || out.Size < 0 || out.Size > 256<<10 {
		return nil, errors.New("heimdall.yaml is not a bounded file")
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil || len(b) > 256<<10 {
		return nil, errors.New("GitHub configuration encoding invalid")
	}
	return b, nil
}
func (c *Client) Permission(ctx context.Context, r RepoRef, login string) (string, error) {
	var out struct {
		Permission string `json:"permission"`
	}
	err := c.do(ctx, r, map[string]string{"metadata": "read"}, "GET", repoPath(r)+"collaborators/"+url.PathEscape(login)+"/permission", nil, &out)
	return out.Permission, err
}
func CanMaintain(permission string) bool {
	return permission == "write" || permission == "maintain" || permission == "admin"
}
func (c *Client) Comment(ctx context.Context, r RepoRef, id int64) (IssueComment, error) {
	var out IssueComment
	err := c.do(ctx, r, map[string]string{"pull_requests": "read"}, "GET", repoPath(r)+"issues/comments/"+strconv.FormatInt(id, 10), nil, &out)
	return out, err
}
func (c *Client) WorkflowRun(ctx context.Context, r RepoRef, id int64) (WorkflowRun, error) {
	var out WorkflowRun
	err := c.do(ctx, r, map[string]string{"actions": "read"}, "GET", repoPath(r)+"actions/runs/"+strconv.FormatInt(id, 10), nil, &out)
	return out, err
}
func (c *Client) ListComments(ctx context.Context, r RepoRef, n int64) ([]IssueComment, error) {
	out := []IssueComment{}
	for page := 1; page <= 10; page++ {
		var batch []IssueComment
		err := c.do(ctx, r, map[string]string{"pull_requests": "read"}, "GET", repoPath(r)+"issues/"+strconv.FormatInt(n, 10)+"/comments?per_page=100&page="+strconv.Itoa(page), nil, &batch)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			return out, nil
		}
	}
	return nil, errors.New("too many PR comments for safe recovery")
}
func (c *Client) WriteComment(ctx context.Context, r RepoRef, n, id int64, body string) (int64, error) {
	var out IssueComment
	method, path := "POST", repoPath(r)+"issues/"+strconv.FormatInt(n, 10)+"/comments"
	if id > 0 {
		method, path = "PATCH", repoPath(r)+"issues/comments/"+strconv.FormatInt(id, 10)
	}
	err := c.do(ctx, r, map[string]string{"pull_requests": "write"}, method, path, map[string]string{"body": body}, &out)
	return out.ID, err
}

type Check struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	ExternalID string `json:"external_id"`
	HeadSHA    string `json:"head_sha"`
	App        struct {
		ID int64 `json:"id"`
	} `json:"app"`
}

func (c *Client) Checks(ctx context.Context, r RepoRef, sha string) ([]Check, error) {
	all := []Check{}
	for page := 1; page <= 10; page++ {
		var out struct {
			CheckRuns []Check `json:"check_runs"`
		}
		err := c.do(ctx, r, map[string]string{"checks": "read"}, "GET", repoPath(r)+"commits/"+sha+"/check-runs?check_name=Heimdall%20Preview&per_page=100&page="+strconv.Itoa(page), nil, &out)
		if err != nil {
			return nil, err
		}
		all = append(all, out.CheckRuns...)
		if len(out.CheckRuns) < 100 {
			return all, nil
		}
	}
	return nil, errors.New("too many checks for safe recovery")
}
func (c *Client) OwnCheck(check Check) bool { return check.App.ID == c.o.AppID }

type CheckUpdate struct {
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha,omitempty"`
	ExternalID string `json:"external_id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	Output     struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
}

func (c *Client) WriteCheck(ctx context.Context, r RepoRef, id int64, u CheckUpdate) (int64, error) {
	var out Check
	method, path := "POST", repoPath(r)+"check-runs"
	if id > 0 {
		method, path = "PATCH", path+"/"+strconv.FormatInt(id, 10)
		u.HeadSHA = ""
	}
	err := c.do(ctx, r, map[string]string{"checks": "write"}, method, path, u, &out)
	return out.ID, err
}
func ValidSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
func (r RepoRef) Validate() error {
	parts := strings.Split(r.FullName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || len(parts[0]) > 39 || len(parts[1]) > 100 || parts[1] == "." || parts[1] == ".." || r.InstallationID <= 0 || r.RepositoryID <= 0 {
		return fmt.Errorf("invalid registered GitHub repository")
	}
	for _, c := range r.FullName {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' && c != '/' {
			return fmt.Errorf("invalid registered GitHub repository")
		}
	}
	return nil
}

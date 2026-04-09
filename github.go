package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type githubClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
	semaphore  chan struct{}
}

type githubAPIError struct {
	Path       string
	StatusCode int
	Message    string
}

func (e *githubAPIError) Error() string {
	return fmt.Sprintf("github api %s returned %d: %s", e.Path, e.StatusCode, e.Message)
}

type repositoryView struct {
	Name         string            `json:"name"`
	URL          string            `json:"url"`
	PullRequests []pullRequestView `json:"pullRequests"`
}

type pullRequestView struct {
	Number             int            `json:"number"`
	Title              string         `json:"title"`
	URL                string         `json:"url"`
	Author             string         `json:"author"`
	AuthorLogin        string         `json:"authorLogin"`
	AuthorURL          string         `json:"authorUrl"`
	Assignees          []reviewerView `json:"assignees"`
	CreatedAt          string         `json:"createdAt"`
	UpdatedAt          string         `json:"updatedAt"`
	LastReviewAt       string         `json:"lastReviewAt"`
	Reviewers          []reviewerView `json:"reviewers"`
	RequestedReviewers []reviewerView `json:"requestedReviewers"`
}

type reviewerView struct {
	Login      string `json:"login"`
	Display    string `json:"display"`
	URL        string `json:"url"`
	AvatarURL  string `json:"avatarUrl"`
	Status     string `json:"status"`
	Requested  bool   `json:"requested"`
	ReviewedAt string `json:"reviewedAt,omitempty"`
}

type githubRepository struct {
	Name       string `json:"name"`
	HTMLURL    string `json:"html_url"`
	Archived   bool   `json:"archived"`
	Disabled   bool   `json:"disabled"`
	Fork       bool   `json:"fork"`
	OpenIssues int    `json:"open_issues_count"`
}

type githubPullRequest struct {
	Number             int          `json:"number"`
	Title              string       `json:"title"`
	HTMLURL            string       `json:"html_url"`
	CreatedAt          time.Time    `json:"created_at"`
	UpdatedAt          time.Time    `json:"updated_at"`
	User               githubUser   `json:"user"`
	Assignees          []githubUser `json:"assignees"`
	RequestedReviewers []githubUser `json:"requested_reviewers"`
	RequestedTeams     []githubTeam `json:"requested_teams"`
}

type githubUser struct {
	Login     string `json:"login"`
	HTMLURL   string `json:"html_url"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

type githubTeam struct {
	Name    string `json:"name"`
	HTMLURL string `json:"html_url"`
	Slug    string `json:"slug"`
}

type githubReview struct {
	State       string     `json:"state"`
	SubmittedAt *time.Time `json:"submitted_at"`
	User        githubUser `json:"user"`
}

const hiddenReviewerLogin = "copilot-pull-request-reviewer[bot]"

func newGitHubClient(token string, concurrency int) *githubClient {
	if concurrency < 1 {
		concurrency = 1
	}

	return &githubClient{
		baseURL: "https://api.github.com",
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		semaphore: make(chan struct{}, concurrency),
	}
}

func (c *githubClient) LoadDashboard(ctx context.Context, org string) ([]repositoryView, []string, error) {
	repos, err := c.listRepositories(ctx, org)
	if err != nil {
		return nil, nil, err
	}

	repositories := make([]repositoryView, 0, len(repos))
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		warnings []string
	)

	for _, repo := range repos {
		if repo.Archived || repo.Disabled || repo.OpenIssues == 0 {
			continue
		}

		repo := repo
		wg.Add(1)
		go func() {
			defer wg.Done()

			pulls, pullErr := c.listPullRequests(ctx, org, repo.Name)
			if pullErr != nil {
				mu.Lock()
				if isSkippableGitHubError(pullErr) {
					warnings = append(warnings, fmt.Sprintf("Skipped %s: %s", repo.Name, pullErr.Error()))
					mu.Unlock()
					return
				}
				if firstErr == nil {
					firstErr = fmt.Errorf("load pull requests for %s: %w", repo.Name, pullErr)
				}
				mu.Unlock()
				return
			}

			prViews, repoWarnings, buildErr := c.buildPullRequestViews(ctx, org, repo.Name, pulls)
			if buildErr != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = buildErr
				}
				mu.Unlock()
				return
			}

			view := repositoryView{
				Name:         repo.Name,
				URL:          repo.HTMLURL,
				PullRequests: prViews,
			}

			mu.Lock()
			warnings = append(warnings, repoWarnings...)
			repositories = append(repositories, view)
			mu.Unlock()
		}()
	}

	wg.Wait()
	sortRepositories(repositories)
	sort.Strings(warnings)
	return repositories, warnings, firstErr
}

func (c *githubClient) buildPullRequestViews(ctx context.Context, org, repo string, pulls []githubPullRequest) ([]pullRequestView, []string, error) {
	views := make([]pullRequestView, len(pulls))
	warnings := make([]string, 0)

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
	)

	for i, pr := range pulls {
		i := i
		pr := pr
		wg.Add(1)
		go func() {
			defer wg.Done()

			reviewers, requestedReviewers, reviewErr := c.loadReviewers(ctx, org, repo, pr)
			if reviewErr != nil {
				mu.Lock()
				if isSkippableGitHubError(reviewErr) {
					warnings = append(warnings, fmt.Sprintf("Skipped review details for %s#%d: %s", repo, pr.Number, reviewErr.Error()))
					reviewers = requestedReviewers
				} else if firstErr == nil {
					firstErr = fmt.Errorf("load reviewers for %s#%d: %w", repo, pr.Number, reviewErr)
				}
				mu.Unlock()
				if !isSkippableGitHubError(reviewErr) {
					return
				}
			}

			views[i] = pullRequestView{
				Number:             pr.Number,
				Title:              pr.Title,
				URL:                pr.HTMLURL,
				Author:             displayName(pr.User),
				AuthorLogin:        pr.User.Login,
				AuthorURL:          pr.User.HTMLURL,
				Assignees:          usersToReviewerViews(pr.Assignees),
				CreatedAt:          pr.CreatedAt.Format(time.RFC3339),
				UpdatedAt:          pr.UpdatedAt.Format(time.RFC3339),
				LastReviewAt:       latestReviewAt(reviewers),
				Reviewers:          reviewers,
				RequestedReviewers: requestedReviewers,
			}
		}()
	}

	wg.Wait()
	if firstErr != nil {
		return nil, warnings, firstErr
	}

	sort.Slice(views, func(i, j int) bool {
		return views[i].UpdatedAt > views[j].UpdatedAt
	})
	return views, warnings, nil
}

func (c *githubClient) loadReviewers(ctx context.Context, org, repo string, pr githubPullRequest) ([]reviewerView, []reviewerView, error) {
	requested := make([]reviewerView, 0, len(pr.RequestedReviewers)+len(pr.RequestedTeams))
	allReviewers := map[string]reviewerView{}

	for _, reviewer := range pr.RequestedReviewers {
		if shouldHideReviewer(reviewer.Login) {
			continue
		}
		view := reviewerView{
			Login:     reviewer.Login,
			Display:   displayName(reviewer),
			URL:       reviewer.HTMLURL,
			AvatarURL: reviewer.AvatarURL,
			Status:    "Requested",
			Requested: true,
		}
		requested = append(requested, view)
		allReviewers["user:"+reviewer.Login] = view
	}

	for _, team := range pr.RequestedTeams {
		view := reviewerView{
			Login:     "@" + team.Slug,
			Display:   team.Name,
			URL:       team.HTMLURL,
			Status:    "Requested",
			Requested: true,
		}
		requested = append(requested, view)
		allReviewers["team:"+team.Slug] = view
	}

	reviews, err := c.listReviews(ctx, org, repo, pr.Number)
	if err != nil {
		if isSkippableGitHubError(err) {
			reviewers := make([]reviewerView, 0, len(allReviewers))
			for _, reviewer := range allReviewers {
				reviewers = append(reviewers, reviewer)
			}
			sort.Slice(reviewers, func(i, j int) bool {
				return strings.ToLower(reviewers[i].Display) < strings.ToLower(reviewers[j].Display)
			})
			return reviewers, requested, err
		}
		return nil, nil, err
	}

	for _, review := range reduceLatestReviews(reviews) {
		if review.User.Login == "" || shouldHideReviewer(review.User.Login) {
			continue
		}
		key := "user:" + review.User.Login
		view := reviewerView{
			Login:      review.User.Login,
			Display:    displayName(review.User),
			URL:        review.User.HTMLURL,
			AvatarURL:  review.User.AvatarURL,
			Status:     humanizeReviewState(review.State),
			ReviewedAt: timeOrEmpty(review.SubmittedAt),
		}
		if existing, ok := allReviewers[key]; ok {
			view.Requested = existing.Requested
		}
		allReviewers[key] = view
	}

	reviewers := make([]reviewerView, 0, len(allReviewers))
	for _, reviewer := range allReviewers {
		reviewers = append(reviewers, reviewer)
	}

	sort.Slice(requested, func(i, j int) bool {
		return strings.ToLower(requested[i].Display) < strings.ToLower(requested[j].Display)
	})
	sort.Slice(reviewers, func(i, j int) bool {
		if reviewers[i].Status == reviewers[j].Status {
			return strings.ToLower(reviewers[i].Display) < strings.ToLower(reviewers[j].Display)
		}
		return reviewers[i].Status < reviewers[j].Status
	})

	return reviewers, requested, nil
}

func reduceLatestReviews(reviews []githubReview) []githubReview {
	latest := map[string]githubReview{}
	for _, review := range reviews {
		if review.User.Login == "" || review.SubmittedAt == nil {
			continue
		}
		if skipReviewState(review.State) {
			continue
		}
		current, found := latest[review.User.Login]
		if !found || current.SubmittedAt == nil || review.SubmittedAt.After(*current.SubmittedAt) {
			latest[review.User.Login] = review
		}
	}

	result := make([]githubReview, 0, len(latest))
	for _, review := range latest {
		result = append(result, review)
	}
	return result
}

func skipReviewState(state string) bool {
	switch strings.ToUpper(state) {
	case "PENDING":
		return true
	default:
		return false
	}
}

func humanizeReviewState(state string) string {
	switch strings.ToUpper(state) {
	case "APPROVED":
		return "Approved"
	case "CHANGES_REQUESTED":
		return "Changes Requested"
	case "COMMENTED":
		return "Commented"
	case "DISMISSED":
		return "Dismissed"
	default:
		state = strings.TrimSpace(strings.ToLower(state))
		if state == "" {
			return "Unknown"
		}
		return strings.ToUpper(state[:1]) + state[1:]
	}
}

func timeOrEmpty(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339)
}

func displayName(user githubUser) string {
	if strings.TrimSpace(user.Name) != "" {
		return user.Name
	}
	if strings.TrimSpace(user.Login) != "" {
		return user.Login
	}
	return "Unknown"
}

func usersToReviewerViews(users []githubUser) []reviewerView {
	views := make([]reviewerView, 0, len(users))
	for _, user := range users {
		if shouldHideReviewer(user.Login) {
			continue
		}
		views = append(views, reviewerView{
			Login:     user.Login,
			Display:   displayName(user),
			URL:       user.HTMLURL,
			AvatarURL: user.AvatarURL,
		})
	}

	sort.Slice(views, func(i, j int) bool {
		return strings.ToLower(views[i].Display) < strings.ToLower(views[j].Display)
	})
	return views
}

func shouldHideReviewer(login string) bool {
	return strings.EqualFold(strings.TrimSpace(login), hiddenReviewerLogin)
}

func latestReviewAt(reviewers []reviewerView) string {
	var latest time.Time
	for _, reviewer := range reviewers {
		if reviewer.ReviewedAt == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, reviewer.ReviewedAt)
		if err != nil {
			continue
		}
		if parsed.After(latest) {
			latest = parsed
		}
	}
	if latest.IsZero() {
		return ""
	}
	return latest.Format(time.RFC3339)
}

func (c *githubClient) listRepositories(ctx context.Context, org string) ([]githubRepository, error) {
	var all []githubRepository
	for page := 1; ; page++ {
		path := fmt.Sprintf("/orgs/%s/repos?per_page=100&page=%d&sort=updated&type=all", url.PathEscape(org), page)
		var batch []githubRepository
		if err := c.getJSON(ctx, path, &batch); err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
	}
	return all, nil
}

func (c *githubClient) listPullRequests(ctx context.Context, org, repo string) ([]githubPullRequest, error) {
	var all []githubPullRequest
	for page := 1; ; page++ {
		path := fmt.Sprintf("/repos/%s/%s/pulls?state=open&sort=updated&direction=desc&per_page=100&page=%d",
			url.PathEscape(org), url.PathEscape(repo), page)
		var batch []githubPullRequest
		if err := c.getJSON(ctx, path, &batch); err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
	}
	return all, nil
}

func (c *githubClient) listReviews(ctx context.Context, org, repo string, number int) ([]githubReview, error) {
	var all []githubReview
	for page := 1; ; page++ {
		path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=100&page=%d",
			url.PathEscape(org), url.PathEscape(repo), number, page)
		var batch []githubReview
		if err := c.getJSON(ctx, path, &batch); err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
	}
	return all, nil
}

func (c *githubClient) getJSON(ctx context.Context, path string, out any) error {
	select {
	case c.semaphore <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.semaphore }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &githubAPIError{
			Path:       path,
			StatusCode: resp.StatusCode,
			Message:    strings.TrimSpace(string(body)),
		}
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func isSkippableGitHubError(err error) bool {
	var apiErr *githubAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusNotFound
}

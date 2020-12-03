package vcs

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/mcdafydd/go-azuredevops/azuredevops"
	"github.com/pkg/errors"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/events/vcs/common"
	"github.com/runatlantis/atlantis/server/logging"
)

// AzureDevopsClient represents an Azure DevOps VCS client
type AzureDevopsClient struct {
	Client   *azuredevops.Client
	ctx      context.Context
	userGUID string
}

// NewAzureDevopsClient returns a valid Azure DevOps client.
func NewAzureDevopsClient(hostname string, token string) (*AzureDevopsClient, error) {
	tp := azuredevops.BasicAuthTransport{
		Username: "",
		Password: strings.TrimSpace(token),
	}
	httpClient := tp.Client()
	httpClient.Timeout = time.Second * 10
	var adClient, err = azuredevops.NewClient(httpClient)
	if err != nil {
		return nil, err
	}

	if hostname != "dev.azure.com" {
		baseURL := fmt.Sprintf("https://%s/", hostname)
		base, err := url.Parse(baseURL)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid azure devops hostname trying to parse %s", baseURL)
		}
		adClient.BaseURL = *base
	}

	client := &AzureDevopsClient{
		Client:   adClient,
		ctx:      context.Background(),
		userGUID: "auto",
	}

	return client, nil
}

// GetModifiedFiles returns the names of files that were modified in the merge request
// relative to the repo root, e.g. parent/child/file.txt.
func (g *AzureDevopsClient) GetModifiedFiles(repo models.Repo, pull models.PullRequest) ([]string, error) {
	var files []string

	opts := azuredevops.PullRequestGetOptions{
		IncludeWorkItemRefs: true,
	}
	owner, project, repoName := SplitAzureDevopsRepoFullName(repo.FullName)
	commitIDResponse, _, _ := g.Client.PullRequests.GetWithRepo(g.ctx, owner, project, repoName, pull.Num, &opts)

	commitID := commitIDResponse.GetLastMergeSourceCommit().GetCommitID()

	r, _, _ := g.Client.Git.GetChanges(g.ctx, owner, project, repoName, commitID)

	for _, change := range r.Changes {
		item := change.GetItem()
		// Convert the path to a relative path from the repo's root.
		relativePath := filepath.Clean("./" + item.GetPath())
		files = append(files, relativePath)

		// If the file was renamed, we'll want to run plan in the directory
		// it was moved from as well.
		changeType := azuredevops.Rename.String()
		if change.ChangeType == &changeType {
			// Convert the path to a relative path from the repo's root.
			relativePath = filepath.Clean("./" + change.GetSourceServerItem())
			files = append(files, relativePath)
		}
	}

	return files, nil
}

// CreateComment creates a comment on a pull request.
//
// If comment length is greater than the max comment length we split into
// multiple comments.
func (g *AzureDevopsClient) CreateComment(repo models.Repo, pullNum int, comment string, command string) error {
	sepEnd := "\n```\n</details>" +
		"\n<br>\n\n**Warning**: Output length greater than max comment size. Continued in next comment."
	sepStart := "Continued from previous comment.\n<details><summary>Show Output</summary>\n\n" +
		"```diff\n"

	// maxCommentLength is the maximum number of chars allowed in a single comment
	// This length was copied from the Github client - haven't found documentation
	// or tested limit in Azure DevOps.
	const maxCommentLength = 65536

	comments := common.SplitComment(comment, maxCommentLength, sepEnd, sepStart)
	owner, project, repoName := SplitAzureDevopsRepoFullName(repo.FullName)

	for _, c := range comments {
		commentType := "text"
		parentCommentID := 0

		prComment := azuredevops.Comment{
			CommentType:     &commentType,
			Content:         &c,
			ParentCommentID: &parentCommentID,
		}
		prComments := []*azuredevops.Comment{&prComment}
		body := azuredevops.GitPullRequestCommentThread{
			Comments: prComments,
		}
		resp, _, err := g.Client.PullRequests.CreateComments(g.ctx, owner, project, repoName, pullNum, &body)
		if err != nil {
			return err
		}
		// When we create a single new comment, the only identity in the thread is us
		log := logging.NewSimpleLogger("azdo", false, logging.Debug).Debug
		if g.userGUID == "auto" {
			log("user guid set to auto")
			if len(resp.Comments) == 1 {
				commentToCacheFrom := resp.Comments[0]
				if commentToCacheFrom != nil && commentToCacheFrom.Author != nil {
					guidToCache := *commentToCacheFrom.Author.ID
					if guidToCache != "" {
						log("Going to cache user GUID as %s", guidToCache)
						g.userGUID = guidToCache
					} else {
						log("GUID to cache is empty")
						log(spew.Sdump(resp))
					}
				} else {
					log("Comment to cahe from is nil or author is nil")
					log(spew.Sdump(resp))
				}
			} else {
				log("User guid set to auto but response identities != 1")
				log(spew.Sdump(resp))
			}
		}
	}
	return nil
}

func (g *AzureDevopsClient) HidePrevPlanComments(repo models.Repo, pullNum int) error {
	return nil
}

// PullIsApproved returns true if the merge request was approved by another reviewer.
// https://docs.microsoft.com/en-us/azure/devops/repos/git/branch-policies?view=azure-devops#require-a-minimum-number-of-reviewers
func (g *AzureDevopsClient) PullIsApproved(repo models.Repo, pull models.PullRequest) (bool, error) {
	owner, project, repoName := SplitAzureDevopsRepoFullName(repo.FullName)

	opts := azuredevops.PullRequestGetOptions{
		IncludeWorkItemRefs: true,
	}
	adPull, _, err := g.Client.PullRequests.GetWithRepo(g.ctx, owner, project, repoName, pull.Num, &opts)
	if err != nil {
		return false, errors.Wrap(err, "getting pull request")
	}

	for _, review := range adPull.Reviewers {
		if review == nil {
			continue
		}

		if review.IdentityRef.GetUniqueName() == adPull.GetCreatedBy().GetUniqueName() {
			continue
		}

		if review.GetVote() == azuredevops.VoteApproved || review.GetVote() == azuredevops.VoteApprovedWithSuggestions {
			return true, nil
		}
	}

	return false, nil
}

// PullIsMergeable returns true if the merge request can be merged.
func (g *AzureDevopsClient) PullIsMergeable(repo models.Repo, pull models.PullRequest) (bool, error) {
	owner, project, repoName := SplitAzureDevopsRepoFullName(repo.FullName)

	opts := azuredevops.PullRequestGetOptions{IncludeWorkItemRefs: true}
	adPull, _, err := g.Client.PullRequests.GetWithRepo(g.ctx, owner, project, repoName, pull.Num, &opts)
	if err != nil {
		return false, errors.Wrap(err, "getting pull request")
	}

	if *adPull.MergeStatus != azuredevops.MergeSucceeded.String() {
		return false, nil
	}

	if *adPull.IsDraft {
		return false, nil
	}

	if *adPull.Status != azuredevops.PullActive.String() {
		return false, nil
	}

	projectID := *adPull.Repository.Project.ID
	artifactID := g.Client.PolicyEvaluations.GetPullRequestArtifactID(projectID, pull.Num)
	policyEvaluations, _, err := g.Client.PolicyEvaluations.List(g.ctx, owner, project, artifactID, &azuredevops.PolicyEvaluationsListOptions{})
	if err != nil {
		return false, errors.Wrap(err, "getting policy evaluations")
	}

	for _, policyEvaluation := range policyEvaluations {
		if !*policyEvaluation.Configuration.IsEnabled || *policyEvaluation.Configuration.IsDeleted {
			continue
		}

		// Ignore the Atlantis status, even if its set as a blocker.
		// This status should not be considered when evaluating if the pull request can be applied.
		settings := (policyEvaluation.Configuration.Settings).(map[string]interface{})
		if genre, ok := settings["statusGenre"]; ok && genre == "Atlantis Bot/atlantis" {
			if name, ok := settings["statusName"]; ok && name == "apply" {
				continue
			}
		}

		if *policyEvaluation.Configuration.IsBlocking && *policyEvaluation.Status != azuredevops.PolicyEvaluationApproved {
			return false, nil
		}
	}

	return true, nil
}

// GetPullRequest returns the pull request.
func (g *AzureDevopsClient) GetPullRequest(repo models.Repo, num int) (*azuredevops.GitPullRequest, error) {
	opts := azuredevops.PullRequestGetOptions{
		IncludeWorkItemRefs: true,
	}
	owner, project, repoName := SplitAzureDevopsRepoFullName(repo.FullName)
	pull, _, err := g.Client.PullRequests.GetWithRepo(g.ctx, owner, project, repoName, num, &opts)
	return pull, err
}

// UpdateStatus updates the build status of a commit.
func (g *AzureDevopsClient) UpdateStatus(repo models.Repo, pull models.PullRequest, state models.CommitStatus, src string, description string, url string) error {
	adState := azuredevops.GitError.String()
	switch state {
	case models.PendingCommitStatus:
		adState = azuredevops.GitPending.String()
	case models.SuccessCommitStatus:
		adState = azuredevops.GitSucceeded.String()
	case models.FailedCommitStatus:
		adState = azuredevops.GitFailed.String()
	}

	status := azuredevops.GitPullRequestStatus{}
	status.Context = GitStatusContextFromSrc(src)
	status.Description = &description
	status.State = &adState
	if url != "" {
		status.TargetURL = &url
	}

	owner, project, repoName := SplitAzureDevopsRepoFullName(repo.FullName)

	opts := azuredevops.PullRequestListOptions{}
	source, resp, err := g.Client.PullRequests.Get(g.ctx, owner, project, pull.Num, &opts)
	if err != nil {
		return errors.Wrap(err, "getting pull request")
	}
	if resp.StatusCode != http.StatusOK {
		return errors.Wrapf(err, "http response code %d getting pull request", resp.StatusCode)
	}
	if source.GetSupportsIterations() {
		opts := azuredevops.PullRequestIterationsListOptions{}
		iterations, resp, err := g.Client.PullRequests.ListIterations(g.ctx, owner, project, repoName, pull.Num, &opts)
		if err != nil {
			return errors.Wrap(err, "listing pull request iterations")
		}
		if resp.StatusCode != http.StatusOK {
			return errors.Wrapf(err, "http response code %d listing pull request iterations", resp.StatusCode)
		}
		for _, iteration := range iterations {
			if sourceRef := iteration.GetSourceRefCommit(); sourceRef != nil {
				if *sourceRef.CommitID == pull.HeadCommit {
					status.IterationID = iteration.ID
					break
				}
			}
		}
		if iterationID := status.IterationID; iterationID != nil {
			if !(*iterationID >= 1) {
				return errors.New("supportsIterations was true but got invalid iteration ID or no matching iteration commit SHA was found")
			}
		}
	}
	_, resp, err = g.Client.PullRequests.CreateStatus(g.ctx, owner, project, repoName, pull.Num, &status)
	if err != nil {
		return errors.Wrap(err, "creating pull request status")
	}
	if resp.StatusCode != http.StatusOK {
		return errors.Wrapf(err, "http response code %d creating pull request status", resp.StatusCode)
	}
	return err
}

// MergePull merges the merge request using the default no fast-forward strategy
// If the user has set a branch policy that disallows no fast-forward, the merge will fail
// until we handle branch policies
// https://docs.microsoft.com/en-us/azure/devops/repos/git/branch-policies?view=azure-devops
func (g *AzureDevopsClient) MergePull(pull models.PullRequest) error {
	if g.userGUID == "auto" {
		// In this case, when we get an error it will comment and then it will be cached
		// However, we should never get this case as we comment "auto merging" before we merge
		return errors.New("User GUID set to auto but hasn't been cached yet. Please try again.")
	} else if g.userGUID == "" {
		// The user GUID must be set for automerge to happen, otherwise:
		// - we get a 400 if the GUID is set to something besides the users' GUID
		// - we get a 200 but it doesn't merge if the GUID is left empty
		return errors.New("User GUID is empty. Try setting --azuredevops-user-guid in atlantis config")
	}

	id := azuredevops.IdentityRef{
		ID: &g.userGUID,
	}
	// Set default pull request completion options
	mcm := azuredevops.NoFastForward.String()
	twi := new(bool)
	*twi = true
	completionOpts := azuredevops.GitPullRequestCompletionOptions{
		BypassPolicy:            new(bool),
		BypassReason:            azuredevops.String(""),
		DeleteSourceBranch:      new(bool),
		MergeCommitMessage:      azuredevops.String(common.AutomergeCommitMsg),
		MergeStrategy:           &mcm,
		SquashMerge:             new(bool),
		TransitionWorkItems:     twi,
		TriggeredByAutoComplete: new(bool),
	}

	owner, project, repoName := SplitAzureDevopsRepoFullName(pull.BaseRepo.FullName)
	mergeResult, _, err := g.Client.PullRequests.Merge(
		g.ctx,
		owner,
		project,
		repoName,
		pull.Num,
		nil,
		completionOpts,
		id,
	)
	if err != nil {
		return errors.Wrap(err, "merging pull request")
	}
	if *mergeResult.MergeStatus != azuredevops.MergeSucceeded.String() {
		return fmt.Errorf("could not merge pull request: %s", mergeResult.GetMergeFailureMessage())
	}
	return nil
}

// MarkdownPullLink specifies the string used in a pull request comment to reference another pull request.
func (g *AzureDevopsClient) MarkdownPullLink(pull models.PullRequest) (string, error) {
	return fmt.Sprintf("!%d", pull.Num), nil
}

// SplitAzureDevopsRepoFullName splits a repo full name up into its owner,
// repo and project name segments. If the repoFullName is malformed, may
// return empty strings for owner, repo, or project.  Azure DevOps uses
// repoFullName format owner/project/repo.
//
// Ex. runatlantis/atlantis => (runatlantis, atlantis)
//     gitlab/subgroup/runatlantis/atlantis => (gitlab/subgroup/runatlantis, atlantis)
//     azuredevops/project/atlantis => (azuredevops, project, atlantis)
func SplitAzureDevopsRepoFullName(repoFullName string) (owner string, project string, repo string) {
	firstSlashIdx := strings.Index(repoFullName, "/")
	lastSlashIdx := strings.LastIndex(repoFullName, "/")
	slashCount := strings.Count(repoFullName, "/")
	if lastSlashIdx == -1 || lastSlashIdx == len(repoFullName)-1 {
		return "", "", ""
	}
	if firstSlashIdx != lastSlashIdx && slashCount == 2 {
		return repoFullName[:firstSlashIdx],
			repoFullName[firstSlashIdx+1 : lastSlashIdx],
			repoFullName[lastSlashIdx+1:]
	}
	return repoFullName[:lastSlashIdx], "", repoFullName[lastSlashIdx+1:]
}

func (g *AzureDevopsClient) SupportsSingleFileDownload(repo models.Repo) bool {
	return false
}

func (g *AzureDevopsClient) DownloadRepoConfigFile(pull models.PullRequest) (bool, []byte, error) {
	return false, []byte{}, fmt.Errorf("Not Implemented")
}

// GitStatusContextFromSrc parses an Atlantis formatted src string into a context suitable
// for the status update API. In the AzureDevops branch policy UI there is a single string
// field used to drive these contexts where all text preceding the final '/' character is
// treated as the 'genre'.
func GitStatusContextFromSrc(src string) *azuredevops.GitStatusContext {
	lastSlashIdx := strings.LastIndex(src, "/")
	genre := "Atlantis Bot"
	name := src
	if lastSlashIdx != -1 {
		genre = fmt.Sprintf("%s/%s", genre, src[:lastSlashIdx])
		name = src[lastSlashIdx+1:]
	}

	return &azuredevops.GitStatusContext{
		Name:  &name,
		Genre: &genre,
	}
}

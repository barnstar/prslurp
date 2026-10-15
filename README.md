# prslurp

Extract unresolved code-review threads from a GitHub PR
into a Markdown file.


It shells out to the `gh` CLI (GraphQL API) because the REST API does not
expose whether a review thread is resolved — only GraphQL
(PullRequest.reviewThreads.isResolved) does.

 Requirements:
   - gh CLI installed and authenticated to the repo's host
     (gh auth login — works with SSH or HTTPS credentials).
   - The repo must be readable by the authenticated user.

## Build (stdlib only, no dependencies):

go build pslurp.go

## Usage (flags may come before or after the PR argument):

	pslurp https://github.com/acme/widgets/pull/482
	pslurp https://github.com/acme/widgets/pull/482 -o feedback.md
	pslurp -repo acme/widgets 482
	pslurp acme/widgets#482 -incl-resolved | head -100

 Flags:

	-repo owner/repo   repository containing the PR (not needed when the
	                   positional arg is a URL or "owner/repo#N")
    -incl-resolved     also include threads that were resolved (tagged as such)
	-incl-outdated     also include threads whose diff position is outdated
	                   (code has moved on; excluded by default)
	-skip-collapsed    skip threads that have replies you haven't viewed yet
	                   (GitHub marks these "collapsed"; included by default)
	-o path            write output here (default:
	                   <owner>-<repo>-pr<number>-unresolved-feedback.md in the
	                   current directory; use "-" for stdout)

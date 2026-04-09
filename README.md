# review-dashboard

A small Go server that shows pull requests for every repository in a GitHub organization, along with requested reviewers and the latest review status for each reviewer.

## Features

- Lists repositories in a GitHub org.
- Shows open pull requests for each repo.
- Shows requested reviewers for each pull request.
- Shows the latest submitted review state for each reviewer.
- Exposes both an HTML dashboard and a JSON API.

## Requirements

- Go 1.23+
- A GitHub token with access to the target organization's repositories

## Configuration

Set these environment variables before starting the server:

```bash
export GITHUB_ORG=your-org
export GITHUB_TOKEN=xxx
export PORT=8080
export GITHUB_CONCURRENCY=8
export GITHUB_CACHE_TTL=2m
export GITHUB_REFRESH_TIMEOUT=90s
```

`PORT`, `GITHUB_CONCURRENCY`, `GITHUB_CACHE_TTL`, and `GITHUB_REFRESH_TIMEOUT` are optional.

## Run

```bash
go run .
```

Then open [http://localhost:8080](http://localhost:8080).

## Endpoints

- `/` renders the dashboard
- `/api/dashboard` returns the same data as JSON
- `/healthz` returns a simple health response

## Container Image

Build the image:

```bash
make
```

Or set the image name explicitly:

```bash
export REPO=ghcr.io/your-org/review-dashboard
export TAG=latest
make
```

This resolves to `docker build -t ${REPO}:${TAG} .`.

Run the container locally:

```bash
docker run --rm -p 8080:8080 \
  -e GITHUB_ORG=your-org \
  -e GITHUB_TOKEN=xxx \
  -e PORT=8080 \
  ${REPO:-review-dashboard}:${TAG:-latest}
```

The provided [Dockerfile](Dockerfile) uses a multi-stage build and ships a minimal non-root runtime image. The [Makefile](Makefile) wraps the image build and push flow.

## GitHub Actions

A GitHub Actions workflow is available at [.github/workflows/build.yml](.github/workflows/build.yml).

It will:

- build the Docker image for pull requests
- build and push the image to GHCR on pushes to `main`
- build and push versioned images on tags like `v1.0.0`

The image will be published to:

```text
ghcr.io/<owner>/<repo>
```

For this workflow to push successfully, make sure:

- GitHub Actions has permission to write packages
- the repository package visibility in GHCR matches your intended usage

## Kubernetes Pod

A sample Pod manifest is available at [k8s/pod.yaml](k8s/pod.yaml).

Create the GitHub token secret:

```bash
kubectl create secret generic review-dashboard \
  --from-literal=github-token=ghp_xxx
```

Apply the Pod manifest:

```bash
kubectl apply -f k8s/pod.yaml
```

Before applying it, update these fields in [k8s/pod.yaml](k8s/pod.yaml):

- `image`
- `GITHUB_ORG`
- optional resource limits and requests

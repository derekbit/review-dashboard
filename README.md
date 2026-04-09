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

The Pod reads `GITHUB_TOKEN` from a Kubernetes Secret with:

- secret name: `review-dashboard`
- key: `github-token`

Create the GitHub token secret:

```bash
kubectl create secret generic review-dashboard \
  --from-literal=github-token=YOUR_GITHUB_TOKEN
```

If you deploy into a namespace, create the secret in that same namespace:

```bash
kubectl create secret generic review-dashboard \
  -n your-namespace \
  --from-literal=github-token=YOUR_GITHUB_TOKEN
```

If the secret already exists, update it with:

```bash
kubectl create secret generic review-dashboard \
  --from-literal=github-token=YOUR_GITHUB_TOKEN \
  --dry-run=client -o yaml | kubectl apply -f -
```

Apply the Pod manifest:

```bash
kubectl apply -f k8s/pod.yaml
```

Expose the Pod with a Service:

```bash
kubectl apply -f k8s/service.yaml
```

If you want users to access the service directly through a node IP, use a NodePort Service instead:

```bash
kubectl apply -f k8s/service-nodeport.yaml
```

Then access it with:

```text
http://<node-ip>:30080
```

You can inspect the node IPs with:

```bash
kubectl get nodes -o wide
```

The provided [k8s/service-nodeport.yaml](k8s/service-nodeport.yaml) exposes the app on node port `30080`.

Expose the Service with an Ingress:

```bash
kubectl apply -f k8s/ingress.yaml
```

For local access through the Service:

```bash
kubectl port-forward svc/review-dashboard 8080:80
```

Then open [http://localhost:8080](http://localhost:8080).

The provided [k8s/service.yaml](k8s/service.yaml) creates a `ClusterIP` Service that selects Pods with label `app=review-dashboard`.

Use [k8s/service-nodeport.yaml](k8s/service-nodeport.yaml) when you want simple access through `node-ip:30080` without setting up an Ingress controller or external load balancer.

The provided [k8s/ingress.yaml](k8s/ingress.yaml) routes HTTP traffic to that Service. Before applying it, update:

- `host` to your real DNS name
- `ingressClassName` if your cluster requires a specific ingress class such as `nginx` or `traefik`

After applying the Ingress, point your DNS record at the ingress controller address. You can inspect it with:

```bash
kubectl get ingress review-dashboard
```

Before applying it, update these fields in [k8s/pod.yaml](k8s/pod.yaml):

- `image`
- `GITHUB_ORG`
- optional resource limits and requests

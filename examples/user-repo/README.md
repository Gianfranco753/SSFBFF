# Example User Repository

This is an example repository structure showing the minimum required files to use SSFBFF with the builder image or GitHub Actions.

## Structure

```
.
├── data/
│   ├── openapi.yaml          # OpenAPI spec with x-service-name extensions
│   ├── proxies.yaml          # Optional: proxy routes
│   ├── services/             # JSONata expressions
│   │   └── example.jsonata
│   └── providers/            # Upstream service configurations
│       └── example_service.yaml
└── .github/
    └── workflows/
        └── build.yml          # GitHub Actions workflow (optional)
```

## Required Files

### `data/openapi.yaml`

Your OpenAPI specification with `x-service-name` extensions:

```yaml
openapi: 3.0.0
info:
  title: My BFF API
  version: 1.0.0
paths:
  /api/v1/example:
    get:
      x-service-name: example
      summary: Get example data
      responses:
        "200":
          description: Success
          content:
            application/json:
              schema:
                type: object
```

### `data/services/example.jsonata`

Your JSONata expression:

```jsonata
{
  "message": "Hello from BFF",
  "data": $fetch("example_service", "endpoint")
}
```

### `data/providers/example_service.yaml`

Upstream service configuration:

```yaml
base_url: http://example-service:8080
timeout: 5s
endpoints:
  endpoint: /api/data
```

## Optional Files

### `data/proxies.yaml`

Pass-through proxy routes:

```yaml
routes:
  - path: /proxy/*
    method: ALL
    url: http://downstream-service:8080
```

### `.github/workflows/build.yml`

Copy from the main SSFBFF repository to enable automatic builds.

## Usage

### Using Docker Builder

```bash
docker run --rm \
  -v $(pwd)/data:/data:ro \
  -v /var/run/docker.sock:/var/run/docker.sock \
  gcossani/ssfbff-builder:latest \
  --output-image my-bff:latest
```

### Using GitHub Actions

1. Copy `.github/workflows/build.yml` from SSFBFF repository
2. Set up repository secrets (DOCKER_USERNAME, DOCKER_PASSWORD, IMAGE_NAME)
3. Push your data directory to GitHub

## Next Steps

1. Replace the example files with your actual API definitions
2. Add your JSONata expressions to `data/services/`
3. Configure your upstream services in `data/providers/`
4. Build and deploy!

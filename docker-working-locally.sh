docker run -e GOOGLE_APPLICATION_CREDENTIALS=/application_default_credentials.json -v "$HOME/.config/gcloud/application_default_credentials.json:/application_default_credentials.json:ro" -v /var/run/docker.sock:/var/run/docker.sock -e PROJECT_ID=$PROJECT_ID --rm opentelemetry-operations-e2e-testing:local local --image=operations-python-e2e-test-server:0.1.4 --gotestflags=-test.v --bind-mount-gcloud=$HOME/.config/gcloud

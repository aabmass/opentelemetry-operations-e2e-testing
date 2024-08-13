# Copyright 2022 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

resource "google_iap_brand" "default" {
  support_email     = "aaronabbott@google.com"
  application_title = "Cloud IAP protected Application"
  project           = var.project_id
}

resource "google_iap_client" "default" {
  display_name = "Test Client"
  brand        =  google_iap_brand.default.name
}

resource "google_app_engine_application" "app" {
  project     = var.project_id
  location_id = "us-central"
  iap {
    enabled = true
    oauth2_client_id = google_iap_client.default.client_id
    oauth2_client_secret = google_iap_client.default.secret
  }
}

import {
  id = var.project_id
  to = google_app_engine_application.app
}

resource "google_app_engine_flexible_app_version" "test" {
  version_id = "v1"
  project    = var.project_id
  service    = "test"
  runtime    = "custom"
  service_account = "${var.project_id}@appspot.gserviceaccount.com"

  deployment {
    container {
      image = "us-central1-docker.pkg.dev/${var.project_id}/gae-service-containers/default-service@sha256:66e0f17efdbef90dc316371ce927afe1a907cd160927fc2e33f99bb8f2eecff3"
    }
  }

  liveness_check {
    path = "/"
  }

  readiness_check {
    path = "/"
  }

  automatic_scaling {
    cool_down_period = "120s"
    cpu_utilization {
      target_utilization = 0.5
    }
  }

  noop_on_destroy = true
}

resource "google_service_account" "push_sa" {
  project = var.project_id
  account_id   = "pubsub-invoker"
  display_name = "Pub Sub push Service Account"
}

resource "google_project_iam_binding" "push_sa_create_token" {
  project = var.project_id
  role    = "roles/iam.serviceAccountTokenCreator"

  members = [
    "serviceAccount:${google_service_account.push_sa.email}",
    "user:aaronabbott@google.com"
  ]
}

data "google_iam_policy" "admin" {
  binding {
    role = "roles/iap.httpsResourceAccessor"
    members = [
      "user:aaronabbott@google.com",
      "serviceAccount:${google_service_account.push_sa.email}"
    ]
  }
}

resource "google_iap_web_iam_policy" "policy" {
  project = var.project_id
  policy_data = data.google_iam_policy.admin.policy_data
}

module "pubsub" {
  source = "../tf/modules/pubsub"
  project_id = var.project_id
}

resource "google_pubsub_subscription" "request_push_subscription" {
  name  = "${module.pubsub.info.request_topic.topic_name}-sub-push4"
  topic = module.pubsub.info.request_topic.topic_name

  ack_deadline_seconds = 60

  message_retention_duration = "1200s"

  push_config {
    push_endpoint = "https://${google_app_engine_flexible_app_version.test.service}-dot-${var.project_id}.uc.r.appspot.com/"

    oidc_token {
      service_account_email = google_service_account.push_sa.email
      audience = google_iap_client.default.client_id
    }
  }
}


# # Lock down the app to only accept internal requests
# resource "google_app_engine_firewall_rule" "rule" {
#   project      = google_app_engine_application.app.project
#   priority     = 1000
#   action       = "ALLOW"
#   source_range = "*"
# }

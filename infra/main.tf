locals {
  image = "${var.region}-docker.pkg.dev/${var.project_id}/${var.artifact_repository}/agentic-loop@sha256:${var.image_digest}"
  apis = toset([
    "artifactregistry.googleapis.com",
    "firestore.googleapis.com",
    "iap.googleapis.com",
    "run.googleapis.com",
    "serviceusage.googleapis.com",
  ])
}

data "google_project" "current" {
  project_id = var.project_id
}

resource "google_project_service" "apis" {
  for_each           = local.apis
  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

resource "google_service_account" "runtime" {
  project      = var.project_id
  account_id   = var.service_account_id
  display_name = "Agentic Loop Cloud Run runtime"
  depends_on   = [google_project_service.apis]
}

# Firestore's default database is the only database managed by this module.
# Named databases do not receive the free quota and are intentionally absent.
resource "google_firestore_database" "default" {
  project     = var.project_id
  name        = "(default)"
  location_id = var.region
  type        = "FIRESTORE_NATIVE"

  deletion_policy = "ABANDON"
  depends_on      = [google_project_service.apis]

  lifecycle {
    prevent_destroy = true
  }
}

resource "google_project_iam_member" "runtime_firestore" {
  project = var.project_id
  role    = "roles/datastore.user"
  member  = "serviceAccount:${google_service_account.runtime.email}"
}

resource "google_cloud_run_v2_service" "app" {
  project             = var.project_id
  name                = var.service_name
  location            = var.region
  deletion_protection = true
  iap_enabled         = true
  # Direct IAP terminates on the Cloud Run URL; no external load balancer is
  # provisioned. IAP remains the only authorized path via IAM below.
  ingress = "INGRESS_TRAFFIC_ALL"

  template {
    service_account                  = google_service_account.runtime.email
    max_instance_request_concurrency = 1

    scaling {
      min_instance_count = 0
      max_instance_count = 1
    }

    containers {
      image = local.image
      resources {
        limits = {
          cpu    = "0.08"
          memory = "512Mi"
        }
        # Request-based billing: CPU is allocated only while handling a
        # request. min_instance_count=0 prevents idle instance cost.
        cpu_idle = true
      }
      ports {
        container_port = 8080
      }
    }
  }

  traffic {
    percent = 100
    type    = "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"
  }

  depends_on = [google_project_service.apis, google_project_iam_member.runtime_firestore]

  lifecycle {
    prevent_destroy = true
    precondition {
      condition     = var.image_digest != ""
      error_message = "A pinned image digest is required; mutable tags are not accepted."
    }
  }
}

# Direct Cloud Run IAP has no external HTTP(S) load balancer in this design.
resource "google_cloud_run_v2_service_iam_member" "iap_service_agent_invoker" {
  project  = var.project_id
  location = google_cloud_run_v2_service.app.location
  name     = google_cloud_run_v2_service.app.name
  role     = "roles/run.invoker"
  member   = "serviceAccount:service-${data.google_project.current.number}@gcp-sa-iap.iam.gserviceaccount.com"

  depends_on = [google_project_service.apis]
}

resource "google_project_iam_member" "iap_accessor" {
  for_each = var.iap_owner_members
  project  = var.project_id
  role     = "roles/iap.httpsResourceAccessor"
  member   = each.value
}

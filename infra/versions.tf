terraform {
  required_version = "= 1.12.5"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "= 7.45.0"
    }
  }

  # Partial backend configuration: the bucket is supplied at init time via
  # -backend-config (scripts/infra-plan.sh and scripts/infra-drift.sh both
  # fail closed if it is unset), so no bucket name is committed here and the
  # local M2 closure can still run scripts/infra-validate.sh's
  # tofu init -backend=false without needing a bucket at all.
  backend "gcs" {}
}

provider "google" {
  project = var.project_id
  region  = var.region
}

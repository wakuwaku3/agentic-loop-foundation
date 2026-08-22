variable "project_id" {
  description = "Existing Google Cloud project. This module never creates a project."
  type        = string
  validation {
    condition     = trimspace(var.project_id) != ""
    error_message = "project_id must name an existing project."
  }
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "service_name" {
  type    = string
  default = "agentic-loop"
}

variable "service_account_id" {
  type    = string
  default = "agentic-loop-runtime"
}

variable "artifact_repository" {
  description = "Existing Artifact Registry repository name; no repository is created here."
  type        = string
  validation {
    condition     = trimspace(var.artifact_repository) != ""
    error_message = "artifact_repository must reference an existing repository."
  }
}

variable "image_digest" {
  description = "Immutable sha256 digest, without the sha256: prefix, for the existing image."
  type        = string
  validation {
    condition     = can(regex("^[a-f0-9]{64}$", var.image_digest))
    error_message = "image_digest must be a 64-character lowercase sha256 digest."
  }
}

variable "iap_owner_members" {
  description = "Explicit IAP allowlist, e.g. user:owner@example.com or group:owners@example.com."
  type        = set(string)
  default     = []
  validation {
    condition     = length(var.iap_owner_members) > 0 && alltrue([for m in var.iap_owner_members : !contains(m, "allUsers") && !contains(m, "allAuthenticatedUsers")])
    error_message = "iap_owner_members must be a non-empty explicit allowlist and may not contain public principals."
  }
}

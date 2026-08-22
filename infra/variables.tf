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

variable "installation_id" {
  description = "Stable installation namespace used for all canonical records."
  type        = string
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,62}$", var.installation_id))
    error_message = "installation_id must be a lowercase, DNS-like identifier."
  }
}

variable "owner_emails" {
  description = "Explicit human owner email allowlist. Each owner also receives IAP access."
  type        = set(string)
  validation {
    condition     = length(var.owner_emails) > 0 && alltrue([for email in var.owner_emails : can(regex("^[^[:space:]@]+@[^[:space:]@]+$", email))])
    error_message = "owner_emails must be a non-empty set of email addresses."
  }
}

variable "owner_origins" {
  description = "Exact HTTPS browser origins allowed to submit owner mutations."
  type        = set(string)
  validation {
    condition     = length(var.owner_origins) > 0 && alltrue([for origin in var.owner_origins : can(regex("^https://[^/]+$", origin))])
    error_message = "owner_origins must contain exact HTTPS origins without paths."
  }
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

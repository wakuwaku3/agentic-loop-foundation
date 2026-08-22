# Loop self-update

The bootstrapper installs signed Runner bundles into immutable
`versions/<version>` directories. `stable` and `preview` are independent
atomic symlink pointers. Installing or switching Preview never overwrites the
Stable binary, and rollback changes only the pointer; both bundles remain until
the separately verified retention window permits garbage collection.

Every manifest binds the binary SHA-256, operating system, architecture, and a
closed schema compatibility interval. Signature, digest, platform, and current
canonical schema are checked before any version directory is published.
Provider credentials and application state are not part of a bundle.

The bootstrapper itself is outside self-update scope. Updating its trust key or
binary is a separate human-approved operation.

---
name: google-aip
description: Design and review gRPC and Protocol Buffer APIs against Google API Improvement Proposals (AIPs). Use when adding or changing .proto resources, RPCs, standard or custom methods, pagination, filtering, field masks, long-running operations, soft delete, versioning, or when an AIP number or compliance review is mentioned. Do not use for protobuf syntax, code generation, or implementation unless AIP guidance is the task.
---

Apply the bundled Google AIPs when designing or reviewing protobuf APIs.

## Workflow

1. Inspect the request and the relevant API definitions.
2. Search `references/catalog.md` for the concepts involved. Use `rg` or another text search instead of loading the entire catalog when possible.
3. Read only the matching documents from `references/aips/{NUMBER}.md`, using four-digit numbers such as `0132.md`.
4. Apply the full AIP text, not the catalog preview. Account for each AIP's state and distinguish normative guidance from examples.
5. When established API precedent conflicts with newer guidance, read AIP-0200 before recommending a deviation.
6. Cite AIP numbers in the result and separate required changes, recommendations, and intentional deviations.

## Resources

- `references/catalog.md`: searchable index with state, title, and a prose preview.
- `references/aips/`: complete local AIP documents.
- `references/provenance.json`: upstream revision, transformation, and license details.

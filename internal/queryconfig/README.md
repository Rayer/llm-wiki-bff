# Query StageConfig v2

The sealed artifact is one strict JSON object with `schema_version=2` and the
production-owned `query_service_implementation` identity
`query-retrieval-pipeline-v2`. Its
Schema v1 is explicitly unsupported and returns `ErrSchemaV1Unsupported`; it is
not migrated because it has no query-service/provider identity. The v2 stage
model identity is explicit for both model stages: `provider`, `model`,
`reasoning`, and `temperature`. Current built-ins require provider `deepseek`,
expansion `deepseek-v4-flash`/`none`/`0`, and synthesis
`deepseek-v4-pro`/`none|low|high|max`/`0`. Credentials and base URLs are not
part of the artifact.

`config_digest` is `sha256:` plus the lowercase full SHA-256 of the normalized
JSON document with the `config_digest` field omitted. Profile catalogs are
sorted by profile ID and project bindings by exact scope; criterion-policy list
order is preserved because profile digests are order-sensitive.

`DecodeStrict` rejects unknown fields, duplicate fields, trailing JSON, and
unsupported identities before validation. `Seal` returns the normalized sealed
copy; `ValidateSealed` verifies its stored digest and all referenced built-ins.

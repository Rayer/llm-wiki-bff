# Query StageConfig v1

The sealed artifact is one strict JSON object with `schema_version=1` and the
production-owned `query_service_implementation` identity
`query-retrieval-pipeline-v2`. Its
`config_digest` is `sha256:` plus the lowercase full SHA-256 of the normalized
JSON document with the `config_digest` field omitted. Profile catalogs are
sorted by profile ID and project bindings by exact scope; criterion-policy list
order is preserved because profile digests are order-sensitive.

`DecodeStrict` rejects unknown fields, duplicate fields, trailing JSON, and
unsupported identities before validation. `Seal` returns the normalized sealed
copy; `ValidateSealed` verifies its stored digest and all referenced built-ins.

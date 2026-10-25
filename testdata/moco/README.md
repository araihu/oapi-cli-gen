# Moco producer fixtures

These are verbatim snapshots of the Moco revision pinned in
`.github/workflows/check.yml`:

- `public.yaml` and `internal.yaml`: principal API bundles without CLI metadata.
- `overlays/cli.*.yaml`: producer-owned OpenAPI Overlay 1.0 naming metadata.
- `cli/public.json` and `cli/internal.json`: CLI input bundles produced by
  Moco's pinned Vacuum bundle/apply-overlay/bundle pipeline.

The demo and generated-client acceptance tests consume `cli/*.json`. The
generator does not merge overlays. Tests verify that the metadata does not
change the base contract, and `make verify-moco-fixtures` compares every fixture
with the pinned producer checkout before the real-server smoke test.

To refresh from a reviewed adjacent Moco checkout:

```sh
make refresh-moco-fixtures MOCO_SOURCE=../moco
make check smoke-moco MOCO_SOURCE=../moco
```

Update the workflow's Moco commit pin together with the fixtures. Edit naming
metadata in Moco's overlays, rebuild there, and refresh these snapshots.

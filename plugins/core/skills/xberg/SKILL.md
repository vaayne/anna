---
name: xberg
description: >-
  Extract text, tables, metadata, and images from 97+ document formats
  (PDF, Office, images, HTML, email, archives, academic) using Xberg CLI.
license: MIT
metadata:
  author: xberg-io
  version: "1.0.14"
  repository: https://github.com/xberg-io/xberg
---

# Xberg Document Extraction

Xberg extracts text, tables, metadata, and images from 97+ file formats. Use it for document processing, OCR, batch extraction, and embeddings.

Run `xberg --help` for all commands, `xberg <command> --help` for full flag reference.

## Core Usage

```bash
# Single file → stdout (text) or JSON
xberg extract document.pdf
xberg extract document.pdf --content-format markdown --format json

# Batch
xberg batch *.pdf --content-format markdown

# Detect MIME type
xberg detect unknown-file
```

## Embeddings

Embeddings require ONNX Runtime. Use a local preset or pipe text through standard input.

```bash
xberg embed --text "hello world" --preset balanced
echo "some text" | xberg embed --preset fast
```

## Chunking

```bash
xberg chunk --text "..." --chunk-size 500 --chunk-overlap 50
xberg chunk --chunker-type markdown --text "# Heading\n\nParagraph..."
cat file.txt | xberg chunk --chunker-type semantic --topic-threshold 0.8
```

## Configuration

Xberg auto-discovers `xberg.toml`, `xberg.yaml`, `xberg.yml`, or `xberg.json` in the current or parent directories. Pass any configuration file explicitly with `--config <path>`.

```bash
xberg extract doc.pdf                          # finds xberg.toml automatically
xberg extract doc.pdf --config my.yaml         # explicit configuration file
xberg extract doc.pdf --config-json '{"ocr":{"language":"deu"}}'
```

Config file skeleton (field names are snake_case — `max_chars` not `max_characters`):

```toml
use_cache = true
enable_quality_processing = true
force_ocr = false

[ocr]
backend = "tesseract"
language = "eng"

[chunking]
max_chars = 1000 # NOT max_characters
max_overlap = 200 # NOT overlap

[pdf_options]
extract_images = true
```

## Extracting Images from PDFs

Images are **not written to disk** — they come back as byte arrays in JSON output. Two-step process required:

```bash
# Step 1: capture JSON
xberg extract doc.pdf --pdf-extract-images true --format json > out.json

# Step 2: save images
python3 -c "
import json, pathlib
d = json.load(open('out.json'))
pathlib.Path('images').mkdir(exist_ok=True)
for img in d.get('images', []):
    pathlib.Path(f'images/image_{img[\"image_index\"]}.{img[\"format\"]}').write_bytes(bytes(img['data']))
"
```

## Key Flags (Non-Obvious)

| Flag                   | Note                                                                                                    |
| ---------------------- | ------------------------------------------------------------------------------------------------------- |
| `--format`             | Wire format for CLI output: `text` (default for extract) or `json`                                      |
| `--content-format`     | Format of extracted text: `plain`, `markdown`, `djot`, `html`. `--output-format` is a deprecated alias. |
| `--token-reduction`    | `off/light/moderate/aggressive/maximum` — reduce tokens before LLM consumption                          |
| `--acceleration`       | ONNX provider: `auto`, `cpu`, `coreml` (macOS), `cuda`, `tensorrt`                                      |
| `--pdf-extract-images` | Embeds image bytes in JSON result (see above)                                                           |

## Common Pitfalls

1. `--format` ≠ `--content-format`: one controls the serialization envelope, the other the text inside it.
2. Config auto-discovery searches `xberg.toml`, `xberg.yaml`, `xberg.yml`, and `xberg.json`.
3. PDF images land in `result.images[]` as byte arrays; nothing is written to disk automatically.
4. `embed` requires ONNX Runtime; install it for your platform before using embeddings.
5. For large documents, use `--token-reduction` to reduce LLM context usage.

## References

- Supported formats: grep `references/supported-formats.md` instead of reading it whole
  e.g. `grep '.mdoc' references/supported-formats.md`

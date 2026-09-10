# print-api

`print-api` is a small, authenticated HTTP API for printing PDF documents on
configured CUPS printers. It exposes declared printer capabilities, print
presets, CUPS printer status, and print-job status through OpenAPI 3.0.

If you want to turn on/off the printer with SwitchBot buttons and Shelly
smart-plugs, see [switch-api](https://github.com/mu373/switch-api).

## Features

- Multiple configured CUPS destinations
- PDF-only uploads with a configurable size limit
- Duplex, color, media, scaling, copies, and minimal-margin options
- Reusable print presets
- CUPS printer and job status endpoints
- API-key authentication and embedded Swagger UI
- Static Linux `amd64` builds with CGO disabled

## Prerequisites

Register each printer as a CUPS destination on the host before starting the
API. The API does not discover printers on the LAN; it only exposes the
destinations declared in `config.json`.

```bash
lpstat -p -d
lpoptions -p PRINTER_QUEUE -l
```

## Configuration

Create a local configuration file from the example.

```bash
cp config.example.json config.json
```

Important fields:

- `default_printer_id`: Printer used when `printer_id` is omitted.
- `cups_destination`: CUPS destination passed to `lp -d`.
- `presets`: Named print options that can be reused by callers.
- `color_modes`: `auto`, `color`, or `monochrome`.
- `duplex_modes`: `none`, `long-edge`, or `short-edge`.
- `media`: Allowed media names for the printer.
- `scaling_modes`: `auto`, `auto-fit`, `fill`, `fit`, or `none`.
- `edge_to_edge`: `unsupported`, `printable-area`, or `full-bleed`.
- `edge_to_edge_options`: Administrator-approved CUPS options added only when
  `edge_to_edge=true`.

`printable-area` minimizes driver-added margins. It does not provide physical
full-bleed printing, and the printer's unprintable margins still apply.

The included `letter-duplex-full` preset selects Letter paper, long-edge
duplex, actual-size scaling, and minimal driver margins. It does not fix a
printer or color mode. Explicit request fields override preset values.

## Authentication

Set `PRINT_API_KEY` to a long random value. All endpoints except `/healthz`,
`/docs/`, and `/openapi.yaml` require it in `X-Api-Key`.

```bash
export PRINT_API_KEY='replace-with-a-long-random-value'
export PRINT_API_CONFIG=config.json
```

## Run

```bash
PRINT_API_KEY='replace-with-a-long-random-value' \
PRINT_API_CONFIG=config.json \
./dist/print-api
```

The default listen address is `:8000`. Set `listen_addr` in `config.json` to
use another address or port, for example `127.0.0.1:9000` for localhost only or
`:9000` for all interfaces. The service currently has no command-line address
or port flag.

## API

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/healthz` | Unauthenticated process health check. |
| `GET` | `/printers` | Configured printers and their declared capabilities. |
| `GET` | `/printers/{printer_id}/status` | Current CUPS state for one configured printer. |
| `GET` | `/presets` | Configured print presets. |
| `POST` | `/print` | Submit a PDF document. |
| `GET` | `/jobs/{job_id}` | Current or terminal CUPS state for a job. |

### Submit a document

```bash
curl \
  -H "X-Api-Key: $PRINT_API_KEY" \
  -F "file=@document.pdf;type=application/pdf" \
  -F "preset=letter-duplex-full" \
  http://localhost:8000/print
```

The response includes a CUPS job ID.

```json
{
  "job_id": "Brother_HL_L2460DW-4",
  "printer_id": "brother-hl-l2460dw",
  "status": "queued"
}
```

### Check printer status

```bash
curl -H "X-Api-Key: $PRINT_API_KEY" \
  http://localhost:8000/printers/brother-hl-l2460dw/status
```

Printer status is one of `ready`, `busy`, `unavailable`, or `unknown`.
`ready` means CUPS considers the destination enabled and idle; it is not a
guarantee that a physical printer has completed every startup operation.

### Check job status

```bash
curl -H "X-Api-Key: $PRINT_API_KEY" \
  http://localhost:8000/jobs/Brother_HL_L2460DW-4
```

Job status is one of `queued`, `processing`, `completed`, `canceled`,
`aborted`, or `unknown`. `unknown` means CUPS has neither a current job nor a
retained terminal record for that ID.

## OpenAPI and Swagger UI

`openapi.yaml` is the source of truth. Server and client code in `internal/oas`
are generated with ogen v1.24.0.

```bash
make generate
```

When the service is running:

- OpenAPI document: `http://localhost:8000/openapi.yaml`
- Swagger UI: `http://localhost:8000/docs/`

Swagger UI assets are embedded in the binary and do not require browser access
to the public internet.

## Build and test

The Makefile pins Go `1.25.3`, Linux `amd64`, and `CGO_ENABLED=0` for release
builds.

```bash
make check
make test
go test -race ./...
make build
make checksum
```

The release binary is written to `dist/print-api`.

## systemd

An example unit is provided in `print-api.service`.

```bash
sudo useradd --system --no-create-home print-api
sudo usermod -aG lp print-api
sudo mkdir -p /opt/print-api
sudo cp dist/print-api config.json /opt/print-api/
echo 'PRINT_API_KEY=replace-with-a-long-random-value' | sudo tee /opt/print-api/.env
sudo chown -R print-api:print-api /opt/print-api
sudo chmod 600 /opt/print-api/.env /opt/print-api/config.json
sudo cp print-api.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now print-api
```

## License

MIT License

# How Platen works

## Printing

Platen talks to printers with **IPP** (Internet Printing Protocol, RFC 8010/8011), the protocol underneath AirPrint, Mopria and IPP Everywhere. An IPP request is an HTTP POST whose body is a small binary message, followed by the document.

1. `Get-Printer-Attributes` tells Platen what the printer is and can do: document formats, paper sizes, duplex, resolutions, ink levels, state.
2. Platen decides how to deliver the document:

   | Document | Printer accepts | What is sent |
   |---|---|---|
   | PDF | `application/pdf` | the PDF, untouched |
   | PDF, text, picture | `image/pwg-raster` only | pages rendered by Platen, as PWG Raster |
   | JPEG | `image/jpeg` | the JPEG, untouched (the printer scales it) |
   | text, picture | `application/pdf` only | an image-only PDF built by Platen |

3. `Print-Job` carries the job options (copies, sides, colour mode, quality, paper) and the document.
4. `Get-Job-Attributes` is polled until the job is done, so the history shows what happened.

### Rendering

PDF pages are rendered by **PDFium**, the engine used by Chrome, compiled to WebAssembly and run inside the Platen process by wazero. There is no cgo and no external program. One page is rendered at a time and streamed to the printer, so memory use does not grow with the length of the document.

Each page is scaled to fit the sheet, centred, and turned by 90 degrees when that matches the paper's orientation.

Quality maps to resolution. Many inkjets accept exactly one raster resolution (600 dpi is common). Platen then renders at a fraction of it and enlarges by an integer factor while encoding, which costs almost nothing:

| Quality | Rendered at | Sent at (600 dpi printer) |
|---|---|---|
| draft | 150 dpi | 600 dpi, each pixel four times |
| normal | 300 dpi | 600 dpi, each pixel twice |
| high | 600 dpi | 600 dpi |

### PWG Raster

PWG Raster (PWG 5102.4) is a simple format: the word `RaS2`, then for each page a 1796-byte header and the pixel rows, run-length encoded. Identical rows are sent once with a repeat count, which is why a mostly white A4 page at 600 dpi (104 MB of pixels) becomes a few hundred kilobytes.

For two-sided printing the printer says how it wants the back of each sheet (`pwg-raster-document-sheet-back`: normal, flipped, rotated or manual-tumble). Platen rotates or mirrors back pages accordingly; without that, every second page comes out upside down.

`internal/raster` contains the encoder and a decoder used by the tests. `platen print -dump job.pwg file.pdf` writes the stream to a file.

## Scanning

Scanners are driven with **eSCL**, the protocol behind AirScan and Mopria Scan. It is HTTP and XML:

1. `GET /eSCL/ScannerCapabilities`: sources, resolutions, colour modes, sizes.
2. `POST /eSCL/ScanJobs` with the scan settings; the answer names the job.
3. `GET <job>/NextDocument` returns a page; a feeder returns one page per request until it is empty.

Two details from real hardware:

- Scanners answer with their mDNS name (`printer-ab12.local`) in the job address. A server often can't resolve that, so Platen keeps the path and uses the host it was configured with.
- Platen sends no "scan intent". With one, some scanners crop the page to what they detect as the document and take several times longer.

Pages are stored as the JPEGs the scanner delivers. A PDF is built by wrapping those JPEGs (`internal/pdfw`), without recompressing them.

## Paperless-ngx

Platen uses the REST API: upload (`/api/documents/post_document/`), follow the import task, read and create tags, correspondents and document types, search, and download a document to print it. Paperless does its own OCR, so Platen sends plain image PDFs.

## Layout of the code

| Package | Role |
|---|---|
| `internal/ipp` | IPP client: printer attributes, print, jobs |
| `internal/raster` | PWG Raster writer (and reader, for tests) |
| `internal/render` | PDF pages, pictures and text to page images |
| `internal/escl` | eSCL client |
| `internal/pdfw` | PDF writer for JPEG pages |
| `internal/paperless` | Paperless-ngx client |
| `internal/store` | scans and print history on disk |
| `internal/hub` | the logic: plans print jobs, runs scan sessions, files documents |
| `internal/api` | REST API, events, access control |
| `internal/web` | the browser interface (plain HTML, CSS and JavaScript, embedded) |
| `internal/mcpserver` | the MCP server and its in-chat views |
| `internal/testutil` | fake printer, scanner and Paperless for tests |

The web interface, the API and the MCP server are thin layers over `internal/hub`, so all three behave the same.

# Platen as an MCP server

Platen speaks the Model Context Protocol in two ways:

| Transport | How to use it | When |
|---|---|---|
| Streamable HTTP at `/mcp` | point the client at `http://<platen>/mcp` | Platen runs as a service; any number of agents share it |
| stdio | the client starts `platen mcp -config platen.yaml` | a single desktop client on the machine that reaches the devices |

Both expose the same tools, prompts and resources.

## Built for the 2026-07-28 revision

The protocol changed a lot in the 2026-07-28 revision, and Platen is written for it:

- **Stateless.** The HTTP endpoint keeps no session. Every request stands alone, so Platen can run with several replicas behind a load balancer. Where a multi-step interaction needs to remember something (a pending confirmation, the scan being built), the handle travels with the request.
- **Multi round-trip requests.** When a tool needs a decision from the person, it does not call the client back. It returns an *input request*; the client asks the person and repeats the call with the answer. Platen uses this for:
  - `print_document`: "Print "Report" on Inkjet? It takes 12 sheets of paper";
  - `scan_document` with `ask_for_more_pages`: "Page 1 is scanned. To add a page, put it on the glass…".
- **Signed request state.** The opaque state that accompanies an input request is signed with HMAC-SHA256 and expires after 30 minutes. For a print confirmation it carries a fingerprint of the job (printer, title, pages, copies, sheets); the confirmation is rejected if the retried call describes a different job. Set `server.state_secret` when running more than one replica, so they accept each other's state.
- **Structured results.** Every tool declares an output schema. Results carry text for the model, structured content for programs, pictures of scanned pages, and a resource link to the PDF instead of the PDF itself.
- **Annotations.** Read-only tools are marked as such; `print_document` and `scan_document` are additive; `cancel_print_job` and `delete_scan` are destructive and idempotent.
- **Cache hints.** The tool, prompt and resource lists carry a five-minute `ttlMs`, and tools are listed in a stable order, which helps clients and prompt caches.
- **MCP Apps.** `printer_status` and `scan_document` reference interactive views (`ui://platen/printer-status.html`, `ui://platen/scan.html`). They are single HTML files with their script and style inline, so they run under the default content security policy without network access. A host that doesn't support MCP Apps ignores them and shows the text result.

Clients on older revisions still work. Over stdio the SDK bridges input requests to classic elicitation. Over stateless HTTP an older client can't be asked anything, so Platen tells the model to ask the person and to repeat the call with `confirm: true`.

## Tools

| Tool | Changes something? | Notes |
|---|---|---|
| `list_devices` | no | start here |
| `printer_status` | no | ink, paper, queue; interactive view |
| `print_document` | prints | exactly one source: `url`, `paperless_document_id`, `scan_id`, `text`, `file_path`, `content_base64` |
| `list_print_jobs` | no | Platen's own history |
| `cancel_print_job` | cancels | |
| `scan_document` | scans, stores the result | returns up to three page pictures and a link to the PDF; interactive view |
| `list_scans` | no | |
| `file_scan_in_paperless` | uploads | waits for Paperless to import the document and returns its address |
| `search_paperless` | no | |
| `delete_scan` | deletes | a copy already filed in Paperless is not touched |

## Prompts

- `scan_and_file`: scan, read, propose title/tags/correspondent/date, file after the person agrees.
- `print_carefully`: check the printer, do a dry run, then print.

## Resources

- `platen://scans/{scan_id}/document`: the PDF of a scan.
- `platen://scans/{scan_id}/pages/{page}`: one page as JPEG.
- `platen://printers/{printer_id}`: printer state as JSON.

## Trying it by hand

With the MCP Inspector:

```sh
npx @modelcontextprotocol/inspector --transport http http://localhost:8080/mcp
```

Or, without any client, the JSON-RPC call for a dry run (2026-07-28 style, no handshake):

```sh
curl -s http://localhost:8080/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -H 'Mcp-Protocol-Version: 2026-07-28' -H 'Mcp-Method: tools/call' -H 'Mcp-Name: print_document' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"print_document",
       "arguments":{"text":"hello","dry_run":true},
       "_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",
                "io.modelcontextprotocol/clientCapabilities":{}}}}'
```

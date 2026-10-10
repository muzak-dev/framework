package tsgen

import (
	"strings"

	"muzak.dev/framework"
)

// runtime is the part of the client that does not depend on the document:
// the options, the error, and the function every method sends its request
// through. It is written once, as it is, and holds nothing from the document.
//
// It refuses a path parameter that would name another route rather than
// escaping it, as [muzak.Endpoint.Call] does. The document writes a trailing
// {name...} wildcard as an ordinary parameter, so the client cannot tell one
// from the other, and holds every parameter to one segment.
const runtime = `/** How createClient reaches the API. */
export interface ClientOptions {
  /** The root the API is served under, such as "https://api.example.com/v1". */
  baseUrl: string;
  /** The fetch requests are sent with, the global one when it is left out. */
  fetch?: typeof fetch;
  /** Headers sent with every request, such as an Authorization. */
  headers?: Record<string, string>;
}

/** A response whose status is not a success, with its body read. */
export class ApiError extends Error {
  /** The status the API answered with. */
  readonly status: number;
  /** The body, parsed when it is JSON and as text otherwise. */
  readonly body: unknown;

  constructor(status: number, body: unknown) {
    super("the API answered with status " + status);
    this.name = "ApiError";
    this.status = status;
    this.body = body;
  }
}

type Fields = Record<string, unknown> | undefined;

interface Sent {
  path?: Fields;
  query?: Fields;
  header?: Fields;
  cookie?: Fields;
}

function eachValue(fields: Fields, add: (name: string, value: string) => void): void {
  if (fields === undefined) {
    return;
  }
  for (const name of Object.keys(fields)) {
    const value: unknown = fields[name];
    const values: unknown[] = Array.isArray(value) ? value : [value];
    for (const item of values) {
      if (item !== undefined && item !== null) {
        add(name, String(item));
      }
    }
  }
}

function encodeBody(kind: string, body: unknown, headers: Headers): BodyInit | undefined {
  if (body === undefined || kind === "none") {
    return undefined;
  }
  if (kind === "json") {
    headers.set("Content-Type", "application/json");
    return JSON.stringify(body);
  }
  if (kind === "blob") {
    return body as Blob;
  }
  const fields = body as Record<string, unknown>;
  if (kind === "urlencoded") {
    const encoded = new URLSearchParams();
    eachValue(fields, (name, value) => encoded.append(name, value));
    return encoded;
  }
  const form = new FormData();
  for (const name of Object.keys(fields)) {
    const value: unknown = fields[name];
    const values: unknown[] = Array.isArray(value) ? value : [value];
    for (const item of values) {
      if (item instanceof Blob) {
        form.append(name, item, name);
      } else if (item !== undefined && item !== null) {
        form.append(name, String(item));
      }
    }
  }
  return form;
}

async function decode(response: Response, kind: string): Promise<unknown> {
  if (kind === "none" || response.status === 204 || response.status === 205) {
    return undefined;
  }
  if (kind === "blob") {
    return response.blob();
  }
  const text = await response.text();
  if (kind === "text") {
    return text;
  }
  return text === "" ? undefined : JSON.parse(text);
}

async function send(
  options: ClientOptions,
  method: string,
  path: string,
  sent: Sent,
  bodyKind: string,
  body: unknown,
  responseKind: string,
  init: RequestInit | undefined,
): Promise<unknown> {
  let base = options.baseUrl;
  while (base.endsWith("/")) {
    base = base.slice(0, -1);
  }
  const url = new URL(base + path);
  eachValue(sent.query, (name, value) => url.searchParams.append(name, value));
  const headers = new Headers(options.headers);
  eachValue(sent.header, (name, value) => headers.append(name, value));
  if (responseKind === "json" && !headers.has("Accept")) {
    headers.set("Accept", "application/json");
  }
  const encoded = encodeBody(bodyKind, body, headers);
  const doFetch = options.fetch ?? fetch;
  const response = await doFetch(url, { ...init, method, headers, body: encoded });
  if (!response.ok) {
    const text = await response.text();
    let parsed: unknown = text;
    try {
      parsed = JSON.parse(text);
    } catch {
      // Not JSON: the text is kept as it is.
    }
    throw new ApiError(response.status, parsed);
  }
  return decode(response, responseKind);
}
`

// pathSegmentFunction escapes one path parameter, and is written only for an
// API with a path parameter, so that a compiler set to refuse unused code
// accepts the file either way.
const pathSegmentFunction = `
function pathSegment(name: string, value: unknown): string {
  const text = value === undefined || value === null ? "" : String(value);
  if (text === "" || text === "." || text === ".." || text.includes("/")) {
    throw new TypeError("the path parameter " + name + " is empty, a dot segment or holds a /, which would name another route");
  }
  return encodeURIComponent(text);
}
`

// client writes the runtime and createClient, with one method per
// operation.
func (g *generator) client() {
	g.out.WriteString("\n")
	g.out.WriteString(runtime)
	for _, o := range g.ops {
		if len(templateParams(o.path)) > 0 {
			g.out.WriteString(pathSegmentFunction)
			break
		}
	}
	g.out.WriteString("\n")
	writeComment(&g.out, "", "Creates a client for the API, with one method per operation.")
	g.out.WriteString("export function createClient(options: ClientOptions) {\n  return {\n")
	for i := range g.ops {
		g.clientMethod(&g.ops[i])
	}
	g.out.WriteString("  };\n}\n")
}

// clientMethod writes the method of one operation.
func (g *generator) clientMethod(o *operation) {
	hasParams := len(o.op.Parameters) > 0
	bodyKind := bodyNone
	if o.op.RequestBody != nil && len(o.op.RequestBody.Content) > 0 {
		_, bodyKind = chooseRequestType(o.op.RequestBody.Content)
	}

	var args []string
	switch {
	case hasParams || bodyKind != bodyNone:
		args = append(args, "params: "+o.base+"Params")
	default:
		args = append(args, "params?: "+o.base+"Params")
	}
	bodyArg := "undefined"
	if bodyKind != bodyNone {
		mark := ""
		if !o.op.RequestBody.Required {
			mark = "?"
		}
		args = append(args, "body"+mark+": "+o.base+"Body")
		bodyArg = "body"
	}
	args = append(args, "init?: RequestInit")
	sent := "params"
	if !hasParams && bodyKind == bodyNone {
		sent = "params ?? {}"
	}

	doc := []string{}
	if o.op.Summary != "" {
		doc = append(doc, o.op.Summary)
	}
	if o.op.Description != "" {
		doc = append(doc, o.op.Description)
	}
	if o.op.Deprecated {
		doc = append(doc, "@deprecated")
	}
	writeComment(&g.out, "    ", doc...)
	g.out.WriteString("    " + o.client + "(" + strings.Join(args, ", ") + "): Promise<" + o.base + "Response> {\n")
	g.out.WriteString("      return send(options, " + quote(o.method) + ", " + pathExpression(o.path) + ", " + sent + ", " +
		quote(bodyKind) + ", " + bodyArg + ", " + quote(successKind(o.op)) + ", init) as Promise<" + o.base + "Response>;\n")
	g.out.WriteString("    },\n")
}

// pathExpression writes the code that builds an operation's path: its static
// text as string literals and each parameter through pathSegment.
func pathExpression(template string) string {
	var parts []string
	// A builder, since adding to a string copies all of it, and a path of
	// many static segments would be copied once per segment.
	var static strings.Builder
	for _, segment := range strings.Split(strings.TrimPrefix(template, "/"), "/") {
		static.WriteByte('/')
		if len(segment) > 2 && segment[0] == '{' && segment[len(segment)-1] == '}' {
			name := segment[1 : len(segment)-1]
			parts = append(parts, quote(static.String()), "pathSegment("+quote(name)+", params.path["+quote(name)+"])")
			static.Reset()
			continue
		}
		writeStatic(&static, segment)
	}
	if static.Len() > 0 {
		parts = append(parts, quote(static.String()))
	}
	return strings.Join(parts, " + ")
}

// writeStatic writes a static segment of a path as the client sends it.
//
// The client joins it to its baseUrl and hands the whole to URL, which reads
// some characters as something other than the path: "?" and "#" begin a query
// and a fragment, a backslash is a "/" in an http URL, and a tab, a carriage
// return and a line feed are dropped. Each of those, and every other control
// character, is written percent-encoded, which a Muzak router decodes before
// it compares a static segment. A dot segment cannot be escaped this way, since
// URL decodes "%2e" too, and is refused instead; see [dotSegment].
func writeStatic(b *strings.Builder, segment string) {
	const hex = "0123456789ABCDEF"
	for i := range len(segment) {
		switch c := segment[i]; {
		case c == '?', c == '#', c == '\\', c < 0x20, c == 0x7f:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		default:
			b.WriteByte(c)
		}
	}
}

// dotSegment returns the first segment of a path template that URL resolves
// as "." or "..", which it does for "%2e" in either case as for a dot.
func dotSegment(template string) (string, bool) {
	for _, segment := range strings.Split(strings.TrimPrefix(template, "/"), "/") {
		if dots := strings.ReplaceAll(strings.ToLower(segment), "%2e", "."); dots == "." || dots == ".." {
			return segment, true
		}
	}
	return "", false
}

// successKind says how the body of an operation's success is read: as JSON
// when any success carries JSON, then as text, then as a Blob, and not at all
// when none carries a body.
func successKind(op *muzak.Operation) string {
	kinds := map[string]bool{}
	for status, response := range op.Responses {
		if !isSuccess(status) || response == nil {
			continue
		}
		for mediaType := range response.Content {
			kinds[responseKind(mediaType)] = true
		}
	}
	for _, kind := range []string{bodyJSON, bodyText, bodyBlob} {
		if kinds[kind] {
			return kind
		}
	}
	return bodyNone
}

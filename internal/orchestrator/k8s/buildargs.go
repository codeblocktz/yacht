package k8s

// declaredArgsAWK prints each name a Dockerfile declares with ARG, once, in
// the order they first appear, from every stage.
//
// awk rather than Go because the Dockerfile is only on the build's volume:
// the Job and the Secret it reads are made before the clone, so the build has
// to choose for itself. busybox has awk, so the BuildKit image needs nothing
// added. It follows the parts of the Dockerfile grammar that decide what is an
// ARG and what is not: the escape directive, line continuations, comments,
// quoting in "ARG A="x y" B", and heredocs, whose bodies are not instructions.
//
// Written to a file by a quoted heredoc rather than passed inline, so the
// program can hold both kinds of quote without the shell reading either.
const declaredArgsAWK = `
BEGIN { esc = "\\"; directives = 1; line = ""; pending = "" }
{
	sub(/\r$/, "")
	# Parser directives come first, before anything else, and "escape" is
	# the one that changes how the rest is read.
	if (directives) {
		if ($0 ~ /^[ \t]*#[ \t]*[A-Za-z]+[ \t]*=/) {
			d = $0; sub(/^[ \t]*#[ \t]*/, "", d)
			k = d; sub(/[ \t]*=.*/, "", k)
			v = d; sub(/^[^=]*=[ \t]*/, "", v)
			if (tolower(k) == "escape" && substr(v, 1, 1) == "` + "`" + `") esc = "` + "`" + `"
			next
		}
		directives = 0
	}
	# A heredoc's body is data, whatever it says, until its own terminator.
	if (pending != "") {
		cur = pending; sub(/ .*/, "", cur)
		t = $0
		if (substr(cur, 1, 1) == "-") { sub(/^\t+/, "", t); cur = substr(cur, 2) }
		if (t == cur) {
			i = index(pending, " ")
			pending = (i ? substr(pending, i + 1) : "")
		}
		next
	}
	# Comments and blank lines go, inside a continuation as well.
	if ($0 ~ /^[ \t]*#/ || $0 ~ /^[ \t]*$/) next
	t = $0; sub(/[ \t]+$/, "", t)
	if (substr(t, length(t), 1) == esc) {
		line = line substr(t, 1, length(t) - 1)
		next
	}
	s = line $0
	line = ""
	instruction(s)
}
END { if (line != "") instruction(line) }

function instruction(s,   kw, rest, i, c, q, w) {
	sub(/^[ \t]+/, "", s)
	kw = s; sub(/[ \t].*/, "", kw)
	kw = toupper(kw)
	if (kw == "RUN" || kw == "COPY" || kw == "ADD") { heredocs(s); return }
	if (kw != "ARG") return
	rest = substr(s, 4)
	w = ""; q = ""
	for (i = 1; i <= length(rest); i++) {
		c = substr(rest, i, 1)
		if (q != "") {
			if (c == esc && q == "\"") i++
			else if (c == q) q = ""
			w = w c
			continue
		}
		if (c == "\"" || c == sq) { q = c; w = w c; continue }
		if (c == esc) { i++; w = w substr(rest, i, 1); continue }
		if (c == " " || c == "\t") { declared(w); w = ""; continue }
		w = w c
	}
	declared(w)
}

function declared(w,   name) {
	if (w == "") return
	name = w; sub(/=.*/, "", name)
	if (name !~ /^[A-Za-z_][A-Za-z0-9_]*$/ || seen[name]++) return
	print name
}

# Queues each heredoc an instruction opens, in order, as its terminator,
# prefixed "-" when leading tabs are stripped from the body.
function heredocs(s,   m, w) {
	while (match(s, "<<-?[\"" sq "]?[A-Za-z_][A-Za-z0-9_]*")) {
		m = substr(s, RSTART, RLENGTH)
		s = substr(s, RSTART + RLENGTH)
		w = m; sub(/^<<-?/, "", w); sub("^[\"" sq "]", "", w)
		if (m ~ /^<<-/) w = "-" w
		pending = (pending == "" ? w : pending " " w)
	}
}
`

// buildArgsScript is the part of the Dockerfile step that turns the app's
// variables into build arguments. It runs in the Dockerfile's directory with
// $dockerfile set, and leaves the flags in the positional parameters, which
// the build passes on as "$@".
//
// Only the names the Dockerfile declares are passed. A build argument a RUN
// uses is kept in the image's history, and BuildKit ignores one nobody
// declared anyway, so handing over the rest would only put them in more
// places.
//
// The values are read from the files the Secret is mounted as and put in the
// positional parameters, which are never word-split, globbed or quoted
// again: a value with spaces, quotes, "=" or newlines arrives as it was
// written. The command substitution would drop trailing newlines, so a
// sentinel character holds them until it is taken off. Nothing here prints a
// value, and the script runs without -x, so the flags never reach the log;
// the names do, so somebody can see which variables the build was given.
const buildArgsScript = `
cat > "${TMPDIR:-/tmp}/declared-args.awk" <<'AWK'
` + declaredArgsAWK + `
AWK
declared=$(awk -v sq="'" -f "${TMPDIR:-/tmp}/declared-args.awk" "$dockerfile")
# Names only, joined on one line. The engine reads this line and keeps it
# out of the log.
echo "` + buildArgsMarker + `" $declared
set --
passed=
for name in $declared; do
  if [ -f "$BUILD_ARGS/$name" ]; then
    value=$(cat "$BUILD_ARGS/$name"; printf x)
    set -- "$@" --opt "build-arg:$name=${value%x}"
    passed="$passed $name"
  fi
done
if [ -n "$passed" ]; then
  echo "Build arguments from this app's variables:$passed"
fi
`

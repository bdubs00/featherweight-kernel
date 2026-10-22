package fw

// This file splits the text of a shell command into the parts that a deny
// or an ask rule of the policy matches (docs/ARCHITECTURE.md section 7.5).
// It is not a shell parser. It reads enough of the shell language to find
// each simple command. Where it is not sure, it gives more parts, so a
// rule matches more often and never less often.

import (
	"path"
	"strings"
)

// Bounds of the split of one command. A command past a bound matches
// every deny and ask rule with a command pattern for its tool.
const (
	// maxCommandDepth bounds the nesting of the lists that the split
	// reads: groups, substitutions, here-documents, and the arguments of
	// a runner.
	maxCommandDepth = 8
	// maxCommandFormBytes bounds the size of all the parts of a command
	// together.
	maxCommandFormBytes = 16 << 20
)

// commandRunners are the commands that run a command from their
// arguments. Each word after a runner can start the command that a rule
// names, and each such word can hold a command line of its own, as the
// argument of "sh -c" does.
var commandRunners = map[string]bool{
	"sudo": true, "doas": true, "su": true, "runuser": true,
	"env": true, "time": true, "nohup": true, "xargs": true,
	"nice": true, "ionice": true, "timeout": true, "stdbuf": true,
	"setsid": true, "chroot": true, "flock": true, "watch": true,
	"command": true, "builtin": true, "exec": true, "eval": true,
	"find": true, "ssh": true,
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
}

// shellSpecial holds the bytes that split or quote a command line.
const shellSpecial = " \t\r\n;&|()<>`$'\"\\"

// commandLeaders are the reserved words of the shell that can come before
// the command of a simple command, as in "if git push; then".
var commandLeaders = map[string]bool{
	"!": true, "{": true, "if": true, "then": true, "elif": true, "else": true,
	"do": true, "while": true, "until": true, "coproc": true,
}

// commandForms returns the texts that a deny or an ask rule with a command
// pattern matches in the shell command cmd, besides cmd itself:
//
//   - Each simple command, without the blanks at its start and its end.
//     The split comes at "&&", "||", ";", "|", "&", and a new line. A
//     separator inside quotes or after a backslash does not split, and
//     neither does a redirection such as "2>&1" or "&>file".
//   - Each simple command inside "( )", "$( )", and backquotes, also
//     inside double quotes, and each line of a here-document.
//   - The text from the command word on. Variable assignments, the
//     reserved words of commandLeaders, and a function header such as
//     "f()" can come before the command word.
//   - After a runner of commandRunners, the text from each later word on.
//     A later word with a blank, a quote, or a special byte also counts
//     as a command line, without its quotes.
//   - Each of these texts without its quotes and backslashes, with one
//     space between its words.
//
// complete is false when the command passes a bound of the split.
func commandForms(cmd string) (forms []string, complete bool) {
	sp := &commandSplitter{}
	sp.list(cmd, 0, 0, 0)
	return sp.forms, !sp.over
}

// commandSplitter collects the forms of one command.
type commandSplitter struct {
	forms []string
	bytes int
	over  bool
}

// shellWord is one word of a simple command: where it starts in the text
// of its list, and its text without quotes and backslashes. A nested part,
// such as "$(date)", stays in the text as it is.
type shellWord struct {
	start int
	text  string
}

// hereDoc is a here-document that waits for the next new line.
type hereDoc struct {
	delim string
	tabs  bool // "<<-" removes the tabs at the start of each line
}

func (sp *commandSplitter) add(s string) {
	s = strings.TrimSpace(s)
	if s == "" || sp.over {
		return
	}
	if sp.bytes += len(s); sp.bytes > maxCommandFormBytes {
		sp.over = true
		return
	}
	sp.forms = append(sp.forms, s)
}

// list reads the command list that starts at s[i] and ends at the byte
// end: 0 for the end of s, ')' for a group or a substitution, and '`' for
// a backquote. It adds the forms of each simple command and returns the
// index of the end byte, or len(s) when the list does not close.
func (sp *commandSplitter) list(s string, i int, end byte, depth int) int {
	if depth > maxCommandDepth {
		sp.over = true
		return len(s)
	}
	var (
		words     []shellWord
		buf       []byte
		inWord    bool
		wordStart int
		docs      []hereDoc
	)
	startWord := func(at int) {
		if !inWord {
			inWord, wordStart = true, at
		}
	}
	endWord := func() {
		if inWord {
			words = append(words, shellWord{start: wordStart, text: string(buf)})
			buf, inWord = buf[:0], false
		}
	}
	endCommand := func(at int) {
		endWord()
		sp.simple(s, words, at, depth)
		words = nil
	}
	// nested reads the list after s[open] up to its end byte, and keeps
	// its whole text in the word.
	nested := func(open int, close byte) int {
		k := sp.list(s, open+1, close, depth+1)
		stop := min(k+1, len(s))
		buf = append(buf, s[open:stop]...)
		return stop
	}
	for i < len(s) {
		c := s[i]
		switch {
		case end != 0 && c == end:
			endCommand(i)
			return i
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
			i++
		case c == '\\':
			if i+1 < len(s) && s[i+1] == '\n' {
				i += 2 // a continued line: the shell joins the two lines
				continue
			}
			startWord(i)
			if i+1 < len(s) {
				buf = append(buf, s[i+1])
				i += 2
				continue
			}
			buf = append(buf, c)
			i++
		case c == '\'':
			startWord(i)
			ansi := len(buf) > 0 && buf[len(buf)-1] == '$' && i > 0 && s[i-1] == '$'
			text, next, ok := singleQuoted(s, i+1, ansi)
			if !ok {
				// A quote that does not close hides no text: it counts
				// as a plain byte.
				buf = append(buf, c)
				i++
				continue
			}
			if ansi {
				buf = buf[:len(buf)-1]
			}
			buf = append(buf, text...)
			i = next
		case c == '"':
			startWord(i)
			text, next, ok := sp.doubleQuoted(s, i+1, depth)
			if !ok {
				buf = append(buf, c)
				i++
				continue
			}
			buf = append(buf, text...)
			i = next
		case c == '(':
			startWord(i)
			i = nested(i, ')')
		case c == '`':
			startWord(i)
			i = nested(i, '`')
		case c == ')':
			// A parenthesis that closes no group, such as the end of a
			// pattern of case, ends a command.
			endCommand(i)
			i++
		case c == ';':
			endCommand(i)
			i++
		case c == '\n':
			endCommand(i)
			i++
			if len(docs) > 0 {
				i = sp.hereDocs(s, i, docs, depth)
				docs = nil
			}
		case c == '|':
			if inWord && i > 0 && s[i-1] == '>' {
				buf = append(buf, c) // ">|" is a redirection
				i++
				continue
			}
			endCommand(i)
			i++
		case c == '&':
			if (inWord && i > 0 && (s[i-1] == '>' || s[i-1] == '<')) || (i+1 < len(s) && s[i+1] == '>') {
				startWord(i)
				buf = append(buf, c) // ">&", "<&", and "&>" are redirections
				i++
				continue
			}
			endCommand(i)
			i++
		case strings.HasPrefix(s[i:], "<<<"):
			startWord(i)
			buf = append(buf, "<<<"...)
			i += 3
		case strings.HasPrefix(s[i:], "<<"):
			startWord(i)
			doc, next := hereDocStart(s, i+2)
			if doc.delim != "" {
				docs = append(docs, doc)
			}
			buf = append(buf, s[i:next]...)
			i = next
		default:
			startWord(i)
			buf = append(buf, c)
			i++
		}
	}
	endCommand(len(s))
	return len(s)
}

// simple adds the forms of one simple command, whose words are words and
// whose text ends at s[end].
func (sp *commandSplitter) simple(s string, words []shellWord, end, depth int) {
	if len(words) == 0 {
		return
	}
	joined, offsets := joinWords(words)
	from := func(k int) {
		raw := strings.TrimSpace(s[words[k].start:end])
		sp.add(raw)
		if norm := joined[offsets[k]:]; norm != raw {
			sp.add(norm)
		}
	}
	for k := 0; k < len(words); {
		from(k)
		w := words[k].text
		switch {
		case isAssignment(w), commandLeaders[w], strings.HasSuffix(w, "()"):
			k++
			continue
		case w == "function":
			k += 2 // "function name" comes before the body
			continue
		case commandRunners[path.Base(w)]:
			for j := k + 1; j < len(words); j++ {
				from(j)
				// A word that holds no blank and no special byte is one
				// word as a command line too, so only the other words
				// can hold a command line of their own.
				if strings.ContainsAny(words[j].text, shellSpecial) {
					sp.list(words[j].text, 0, 0, depth+1)
				}
			}
		}
		break
	}
}

// joinWords joins the texts of words with one space and returns the
// offset of each word in the result.
func joinWords(words []shellWord) (string, []int) {
	var b strings.Builder
	offsets := make([]int, len(words))
	for k, w := range words {
		if k > 0 {
			b.WriteByte(' ')
		}
		offsets[k] = b.Len()
		b.WriteString(w.text)
	}
	return b.String(), offsets
}

// isAssignment reports whether a word assigns a variable, as in
// "FOO=bar" or "PATH+=:/x".
func isAssignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	if !ok {
		return false
	}
	return isFieldName(strings.TrimSuffix(name, "+"))
}

// singleQuoted reads the text of a single-quoted string that starts at
// s[i], after its quote. In an ANSI-C string ($'...'), a backslash keeps
// the next byte, so "\'" does not close it. It returns the index after
// the closing quote, and ok false when the string does not close.
func singleQuoted(s string, i int, ansi bool) (text []byte, next int, ok bool) {
	for k := i; k < len(s); k++ {
		switch {
		case s[k] == '\'':
			return text, k + 1, true
		case ansi && s[k] == '\\' && k+1 < len(s):
			k++
			text = append(text, s[k])
		default:
			text = append(text, s[k])
		}
	}
	return nil, len(s), false
}

// doubleQuoted reads the text of a double-quoted string that starts at
// s[i], after its quote. A substitution inside it runs, so its commands
// count too. It returns the index after the closing quote, and ok false
// when the string does not close.
func (sp *commandSplitter) doubleQuoted(s string, i, depth int) (text []byte, next int, ok bool) {
	for i < len(s) {
		c := s[i]
		switch {
		case c == '"':
			return text, i + 1, true
		case c == '\\' && i+1 < len(s):
			switch s[i+1] {
			case '\n':
			case '"', '\\', '$', '`':
				text = append(text, s[i+1])
			default:
				text = append(text, c, s[i+1])
			}
			i += 2
		case c == '`':
			k := sp.list(s, i+1, '`', depth+1)
			stop := min(k+1, len(s))
			text = append(text, s[i:stop]...)
			i = stop
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			k := sp.list(s, i+2, ')', depth+1)
			stop := min(k+1, len(s))
			text = append(text, s[i:stop]...)
			i = stop
		default:
			text = append(text, c)
			i++
		}
	}
	return nil, len(s), false
}

// hereDocStart reads the delimiter of a here-document after "<<" at s[i]:
// an optional "-", blanks, and a word, whose quotes the delimiter loses.
// It returns the index after the word.
func hereDocStart(s string, i int) (hereDoc, int) {
	var doc hereDoc
	if i < len(s) && s[i] == '-' {
		doc.tabs = true
		i++
	}
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	var delim []byte
	for i < len(s) && !strings.ContainsRune(" \t\r\n;&|<>()`", rune(s[i])) {
		switch s[i] {
		case '\'', '"':
			q := s[i]
			k := strings.IndexByte(s[i+1:], q)
			if k < 0 {
				delim = append(delim, s[i+1:]...)
				return hereDoc{delim: string(delim), tabs: doc.tabs}, len(s)
			}
			delim = append(delim, s[i+1:i+1+k]...)
			i += k + 2
		case '\\':
			if i+1 < len(s) {
				delim = append(delim, s[i+1])
			}
			i += 2
		default:
			delim = append(delim, s[i])
			i++
		}
	}
	doc.delim = string(delim)
	return doc, min(i, len(s))
}

// hereDocs reads the bodies of the here-documents docs from s[i], the
// start of the line after their command, and returns the index after the
// last delimiter line. Each line of a body counts as a command line of its
// own: a body without quotes on its delimiter runs its substitutions, and
// text that a here-document writes to a file can run later. A quote does
// not reach past its line.
func (sp *commandSplitter) hereDocs(s string, i int, docs []hereDoc, depth int) int {
	for _, d := range docs {
		for i < len(s) {
			line, next := s[i:], len(s)
			if nl := strings.IndexByte(line, '\n'); nl >= 0 {
				line, next = line[:nl], i+nl+1
			}
			i = next
			key := line
			if d.tabs {
				key = strings.TrimLeft(key, "\t")
			}
			if key == d.delim {
				break
			}
			sp.list(line, 0, 0, depth+1)
		}
	}
	return i
}

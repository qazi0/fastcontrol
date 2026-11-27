"use strict";

// Presentation only. Keep the original transcript and provider input intact.
const MessageContent = (() => {
  const tags = new Set(["send_user_message_question_reply", "environment_context", "external_codex_apps_open_page", "heartbeat", "INSTRUCTIONS", "local-command-caveat", "command-name", "command-args", "command-message", "local-command-stdout", "system-reminder", "task-notification", "pasted_content"]);
  const agyDropBlockTags = new Set(["ADDITIONAL_METADATA", "USER_SETTINGS_CHANGE", "SYSTEM_MESSAGE", "EPHEMERAL_MESSAGE", "ATTACHED_FILES", "CURRENT_EDITOR", "OPEN_FILES", "USER_CONTEXT", "CONTEXT", "SKILL"]);
  const agyStrictUnclosedDropTags = new Set(["ADDITIONAL_METADATA", "USER_SETTINGS_CHANGE", "SYSTEM_MESSAGE", "EPHEMERAL_MESSAGE", "ATTACHED_FILES", "CURRENT_EDITOR", "OPEN_FILES", "USER_CONTEXT"]);
  const agyBoundaryTags = new Set(["BEGIN_CHAT", "END_CHAT", "BEGIN-CHAT", "END-CHAT"]);
  const agyPartialTagPrefixes = [
    "</USER_REQUEST>", "<USER_REQUEST>",
    "<ADDITIONAL_METADATA>", "</ADDITIONAL_METADATA>",
    "<USER_SETTINGS_CHANGE>", "</USER_SETTINGS_CHANGE>",
    "<SYSTEM_MESSAGE>", "</SYSTEM_MESSAGE>",
    "<EPHEMERAL_MESSAGE>", "</EPHEMERAL_MESSAGE>",
    "<ATTACHED_FILES>", "</ATTACHED_FILES>",
    "<CURRENT_EDITOR>", "</CURRENT_EDITOR>",
    "<OPEN_FILES>", "</OPEN_FILES>",
    "<USER_CONTEXT>", "</USER_CONTEXT>",
    "<CONTEXT>", "</CONTEXT>",
    "<SKILL>", "</SKILL>",
    "<BEGIN_CHAT>", "</BEGIN_CHAT>",
    "<END_CHAT>", "</END_CHAT>",
    "<BEGIN-CHAT>", "</BEGIN-CHAT>",
    "<END-CHAT>", "</END-CHAT>",
  ];

  function isAgyEnvelopeTag(upper) {
    return upper === "USER_REQUEST" || agyBoundaryTags.has(upper) || agyDropBlockTags.has(upper);
  }
  function updateFence(line, fence) {
    const m = line.match(/^\s{0,3}(`{3,}|~{3,})(.*)$/);
    if (!m) return { fence, isFence: false };
    const mark = m[1], rest = m[2];
    if (!fence) {
      if (mark[0] === "`" && rest.includes("`")) return { fence: null, isFence: false };
      return { fence: mark, isFence: true };
    }
    if (mark[0] === fence[0] && mark.length >= fence.length && !rest.trim()) return { fence: null, isFence: true };
    return { fence, isFence: false };
  }
  function parseTagAt(s, pos) {
    if (pos < 0 || pos >= s.length || s[pos] !== "<") return null;
    const sub = s.slice(pos, pos + 256);
    const m = sub.match(/^<(\/?)([A-Za-z][A-Za-z0-9_-]{0,63})(?:([ \t]*)>|([ \t/][^<\n\r]*)>)/);
    if (!m) return null;
    const closing = m[1] === "/";
    if (closing && m[4] !== undefined) return null;
    return { name: m[2].toUpperCase(), closing, end: pos + m[0].length };
  }
  function advanceInlineBackticks(line, idx) {
    let tickLen = 0;
    while (idx + tickLen < line.length && line[idx + tickLen] === "`") tickLen++;
    let i = idx + tickLen;
    while (i < line.length) {
      if (line[i] === "`") {
        let run = 0;
        while (i + run < line.length && line[i + run] === "`") run++;
        if (run === tickLen) return i + run;
        i += run;
        continue;
      }
      i++;
    }
    return idx + tickLen;
  }
  function indexInInlineBackticks(line, target) {
    for (let i = 0; i < line.length && i < target;) {
      if (line[i] === "`") {
        const end = advanceInlineBackticks(line, i);
        if (target > i && target < end) return true;
        i = end;
        continue;
      }
      i++;
    }
    return false;
  }
  function isChatBoundaryLine(line) {
    let s = line.trim();
    if (!s || s.length > 80) return false;
    if (s.startsWith("<!--") && s.endsWith("-->")) s = s.slice(4, -3).trim();
    s = s.replace(/^[-=*#<>\[\(\s]+/, "").replace(/[-=*#<>\[\]\)\s]+$/, "").trim();
    if (!s) return false;
    const parts = s.toUpperCase().replace(/[_-]+/g, " ").split(/\s+/);
    return parts.length === 2 && (parts[0] === "BEGIN" || parts[0] === "END") && parts[1] === "CHAT";
  }
  function isSystemPreambleLine(line) {
    const low = line.trim().toLowerCase();
    return low.startsWith("the following is a <system_message> not actually sent by the user") ||
      low.startsWith("the following system message was injected by a sessionstart hook");
  }
  function nextNonBlankStartsWithTag(s, pos, targetUpper) {
    while (pos < s.length) {
      const nl = s.indexOf("\n", pos);
      const lineEnd = nl >= 0 ? nl : s.length;
      const trimmed = s.slice(pos, lineEnd).trim();
      if (!trimmed) {
        if (nl < 0) return false;
        pos = nl + 1;
        continue;
      }
      const parsed = parseTagAt(trimmed, 0);
      return !!(parsed && !parsed.closing && parsed.name === targetUpper);
    }
    return false;
  }
  function isPartialEnvelopeTag(text) {
    let s = String(text || "").trim();
    if (s.endsWith("\u2026")) s = s.slice(0, -1).trim();
    else if (s.endsWith("...")) s = s.slice(0, -3).trim();
    if (s.length < 2 || s[0] !== "<" || /[>\n\r]/.test(s)) return false;
    const upper = s.toUpperCase();
    return agyPartialTagPrefixes.some(full => full.startsWith(upper) && upper.length < full.length);
  }
  function startsWithEnvelopeMarker(rest) {
    const trimmed = rest.trim();
    if (!trimmed) return false;
    if (isChatBoundaryLine(trimmed)) return true;
    if (trimmed[0] === "<") {
      const parsed = parseTagAt(trimmed, 0);
      if (parsed && isAgyEnvelopeTag(parsed.name)) return true;
      if (isPartialEnvelopeTag(trimmed)) return true;
    }
    return false;
  }
  function findClosingTag(s, start, targetUpper, strictFollowing) {
    let fence = null, depth = 1, pos = start;
    while (pos < s.length) {
      const nl = s.indexOf("\n", pos);
      const lineEnd = nl >= 0 ? nl : s.length;
      const nextLine = nl >= 0 ? nl + 1 : s.length;
      const line = s.slice(pos, lineEnd);
      const atRealLineStart = pos === 0 || s[pos - 1] === "\n";
      if (atRealLineStart) {
        const res = updateFence(line, fence);
        fence = res.fence;
        if (res.isFence || fence) { pos = nextLine; continue; }
      } else if (fence) {
        pos = nextLine; continue;
      }
      for (let i = 0; i < line.length;) {
        if (line[i] === "`") { i = advanceInlineBackticks(line, i); continue; }
        if (line[i] === "<") {
          const parsed = parseTagAt(line, i);
          if (parsed && parsed.name === targetUpper) {
            if (!parsed.closing) {
              if (!line.slice(0, i).trim()) depth++;
              i = parsed.end;
              continue;
            }
            if (depth > 1) {
              depth--;
              i = parsed.end;
              continue;
            }
            if (!strictFollowing) return { start: pos + i, end: pos + parsed.end };
            const rest = line.slice(parsed.end).trim();
            if (!rest || startsWithEnvelopeMarker(rest)) return { start: pos + i, end: pos + parsed.end };
            i = parsed.end;
            continue;
          }
        }
        i++;
      }
      pos = nextLine;
    }
    return null;
  }
  function trimTrailingPartialTag(s) {
    const trimmed = s.replace(/[ \t]+$/, "");
    const cut = trimmed.lastIndexOf("<");
    if (cut < 0 || indexInInlineBackticks(trimmed, cut)) return s;
    if (isPartialEnvelopeTag(trimmed.slice(cut))) return trimmed.slice(0, cut).replace(/[ \t]+$/, "");
    return s;
  }
  function trimTrailingBoundaryTag(line) {
    let work = line;
    for (;;) {
      const trimmed = work.replace(/[ \t]+$/, "");
      if (!trimmed || trimmed[trimmed.length - 1] !== ">") return work;
      const cut = trimmed.lastIndexOf("<");
      if (cut < 0 || indexInInlineBackticks(trimmed, cut)) return work;
      const parsed = parseTagAt(trimmed, cut);
      if (!parsed || parsed.end !== trimmed.length || !agyBoundaryTags.has(parsed.name)) return work;
      work = trimmed.slice(0, cut).replace(/[ \t]+$/, "");
    }
  }
  function stripChatBoundaries(text) {
    const lines = text.split("\n"), out = [];
    let fence = null;
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      if (fence) {
        fence = updateFence(line, fence).fence;
        out.push(line);
        continue;
      }
      const res = updateFence(line, null);
      if (res.isFence) {
        fence = res.fence;
        out.push(line);
        continue;
      }
      if (isChatBoundaryLine(line)) continue;
      let work = line;
      for (;;) {
        const trimmed = work.replace(/^[ \t]+/, "");
        const parsed = parseTagAt(trimmed, 0);
        if (parsed && agyBoundaryTags.has(parsed.name)) {
          work = trimmed.slice(parsed.end);
          continue;
        }
        break;
      }
      work = trimTrailingBoundaryTag(work);
      if (!work.trim() && line.trim()) continue;
      if (i === lines.length - 1 && work.trim()) work = trimTrailingPartialTag(work);
      out.push(work);
    }
    return out.join("\n").trim();
  }
  function cleanUnclosedUserRequest(text) {
    const lines = text.split("\n"), out = [];
    let fence = null;
    for (const line of lines) {
      if (fence) {
        fence = updateFence(line, fence).fence;
        out.push(line);
        continue;
      }
      const res = updateFence(line, null);
      if (res.isFence) {
        fence = res.fence;
        out.push(line);
        continue;
      }
      const trimmed = line.trim();
      if (trimmed.startsWith("<")) {
        const parsed = parseTagAt(trimmed, 0);
        if (parsed && !parsed.closing && agyStrictUnclosedDropTags.has(parsed.name)) break;
      }
      out.push(line);
    }
    return trimTrailingPartialTag(stripChatBoundaries(out.join("\n"))).trim();
  }
  function skipBlankLineTail(s, pos) {
    if (pos >= s.length) return s.length;
    const nl = s.indexOf("\n", pos);
    const tailEnd = nl >= 0 ? nl : s.length;
    if (!s.slice(pos, tailEnd).trim()) return nl >= 0 ? tailEnd + 1 : s.length;
    return pos;
  }
  function cleanUserMessage(raw, streaming = false) {
    const input = String(raw || "");
    if (!input.trim()) return "";
    const s = input.replace(/\r\n/g, "\n").replace(/\r/g, "\n");
    const userRequests = [];
    let plain = "", sawEnvelope = false, sawUserReq = false, fence = null, pos = 0, atLineStart = true;

    while (pos < s.length) {
      const nl = s.indexOf("\n", pos);
      const lineEnd = nl >= 0 ? nl : s.length;
      const nextLine = nl >= 0 ? nl + 1 : s.length;
      const seg = s.slice(pos, lineEnd);

      if (fence) {
        if (atLineStart) fence = updateFence(seg, fence).fence;
        plain += seg + (nl >= 0 ? "\n" : "");
        pos = nextLine;
        atLineStart = true;
        continue;
      }
      if (atLineStart) {
        const res = updateFence(seg, null);
        if (res.isFence) {
          fence = res.fence;
          plain += seg + (nl >= 0 ? "\n" : "");
          pos = nextLine;
          atLineStart = true;
          continue;
        }
        if (isChatBoundaryLine(seg)) {
          sawEnvelope = true;
          pos = nextLine;
          atLineStart = true;
          continue;
        }
        if (isSystemPreambleLine(seg) && nextNonBlankStartsWithTag(s, nextLine, "SYSTEM_MESSAGE")) {
          sawEnvelope = true;
          pos = nextLine;
          atLineStart = true;
          continue;
        }
      }

      let scanPos = pos;
      while (scanPos < lineEnd && (s[scanPos] === " " || s[scanPos] === "\t")) scanPos++;
      if (scanPos < lineEnd && s[scanPos] === "<") {
        const parsed = parseTagAt(s, scanPos);
        if (parsed && isAgyEnvelopeTag(parsed.name)) {
          if (parsed.name === "USER_REQUEST" && !parsed.closing) {
            sawEnvelope = true;
            sawUserReq = true;
            const closed = findClosingTag(s, parsed.end, "USER_REQUEST", true);
            if (closed) {
              const inner = stripChatBoundaries(s.slice(parsed.end, closed.start));
              if (inner) userRequests.push(inner);
              pos = skipBlankLineTail(s, closed.end);
              atLineStart = true;
              continue;
            }
            const inner = cleanUnclosedUserRequest(s.slice(parsed.end));
            if (inner) userRequests.push(inner);
            pos = s.length;
            continue;
          }
          if (parsed.name === "USER_REQUEST" && parsed.closing) {
            const rest = s.slice(parsed.end, lineEnd).trim();
            if (sawEnvelope || !rest || startsWithEnvelopeMarker(rest)) {
              sawEnvelope = true;
              pos = skipBlankLineTail(s, parsed.end);
              atLineStart = true;
              continue;
            }
          }
          if (agyBoundaryTags.has(parsed.name)) {
            sawEnvelope = true;
            pos = skipBlankLineTail(s, parsed.end);
            atLineStart = true;
            continue;
          }
          if (agyDropBlockTags.has(parsed.name) && !parsed.closing) {
            const closed = findClosingTag(s, parsed.end, parsed.name, false);
            if (closed) {
              sawEnvelope = true;
              pos = skipBlankLineTail(s, closed.end);
              atLineStart = true;
              continue;
            }
            const aloneOnLine = !s.slice(parsed.end, lineEnd).trim();
            if (agyStrictUnclosedDropTags.has(parsed.name) || sawEnvelope || aloneOnLine || streaming) {
              sawEnvelope = true;
              pos = s.length;
              continue;
            }
          }
          if (agyDropBlockTags.has(parsed.name) && parsed.closing && sawEnvelope) {
            pos = skipBlankLineTail(s, parsed.end);
            atLineStart = true;
            continue;
          }
        }
        if ((sawEnvelope || streaming) && nl < 0 && isPartialEnvelopeTag(s.slice(scanPos).trim())) {
          sawEnvelope = true;
          pos = s.length;
          continue;
        }
      }

      let outSeg = seg;
      if (sawEnvelope) {
        outSeg = trimTrailingBoundaryTag(outSeg);
        if (nl < 0) outSeg = trimTrailingPartialTag(outSeg);
      }
      plain += outSeg + (nl >= 0 ? "\n" : "");
      pos = nextLine;
      atLineStart = true;
    }

    if (!sawEnvelope) return input.trim();
    if (sawUserReq) return userRequests.join("\n\n").trim();
    return plain.trim();
  }

  function trimClosingPrefix(text, name) {
    const close = '</' + name + '>';
    for (let n = close.length - 1; n > 0; n--) if (text.endsWith(close.slice(0,n))) return text.slice(0,-n);
    return text;
  }
  function field(body, name, partial = false) {
    const match = body.match(new RegExp("<" + name + ">([\\s\\S]*?)(?:</" + name + ">" + (partial ? "|$" : "") + ")"));
    return match ? (partial && !match[0].endsWith('</' + name + '>') ? trimClosingPrefix(match[1], name) : match[1]).trim() : "";
  }
  function parse(text, depth = 0, streaming = false) {
    if (depth >= 8) return [{kind:"text", text:"Additional Session Context"}];
    if (depth === 0) text = cleanUserMessage(text, streaming);
    const parts = [];
    let plain = [], fence = null;
    const flush = () => { const value = plain.join("\n").trim(); if (value) parts.push({kind:"text", text:value}); plain = []; };
    const lines = String(text || "").split("\n");
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i], code = line.match(/^\s{0,3}(`{3,}|~{3,})/);
      if (code) {
        if (!fence) fence = code[1];
        else if (code[1][0] === fence[0] && code[1].length >= fence.length) fence = null;
        plain.push(line); continue;
      }
      const open = !fence && line.match(/^\s*<([\w-]+)>/);
      if (!open || !tags.has(open[1])) {
        const prefix = !fence && streaming && line.match(/^\s*<([\w-]*)$/);
        if (prefix && [...tags].some(tag => tag.startsWith(prefix[1]))) continue;
        plain.push(line); continue;
      }
      const tag = open[1], close = "</" + tag + ">";
      let end = i;
      while (end < lines.length && !lines[end].includes(close)) end++;
      const partial = end === lines.length;
      if (partial && !streaming) { plain.push(line); continue; }
      const block = lines.slice(i, end + 1).join("\n"), at = partial ? block.length : block.indexOf(close);
      const raw = block.slice(block.indexOf(">") + 1, at);
      const body = (partial ? trimClosingPrefix(raw,tag) : raw).trim();
      flush(); i = end;
      if (tag === "send_user_message_question_reply") {
        let replies;
        try { replies = JSON.parse(body); } catch {}
        if (Array.isArray(replies) && replies.length && replies.every(r => r && typeof r.question === "string" && typeof r.answer === "string")) {
          for (const reply of replies) parts.push({kind:"reply", question:reply.question, answer:reply.answer});
        } else if (!partial) parts.push({kind:"text", text:"Answered a question"});
      } else if (tag === "external_codex_apps_open_page") {
        let page;
        try {page = JSON.parse(body);}catch {}
        parts.push({kind:"context", title:page?.page_id === null ? "Page Closed" : page?.page_id ? "Opened a Page" : "Page Context Updated", text:""});
      } else if (tag === "environment_context") {
        const labels = {cwd:"Working Directory", shell:"Shell", current_date:"Date", timezone:"Time Zone"};
        const rows = Object.entries(labels).map(([key,label]) => { const value = field(body,key); return value ? label + ": " + value : ""; }).filter(Boolean);
        parts.push({kind:"context", title:"Session Context", text:rows.join("\n")});
      } else if (tag === "heartbeat") {
        if (body.includes('<message>')) parts.push(...parse(field(body,"message",partial),depth + 1,streaming));
        else if (!partial || body.includes('<instructions>')) {
          const instructions = field(body,"instructions",partial);
          if (instructions || !body.includes('<decision>')) parts.push({kind:"context", title:"Scheduled Check", text:instructions});
        }
      } else if (tag === "task-notification") {
        const status = field(body,"status");
        const title = {completed:"Task Completed", failed:"Task Failed", stopped:"Task Stopped", running:"Task Running"}[status] || "Task Update";
        parts.push({kind:"context", title, text:[field(body,"summary",partial),field(body,"result",partial)].filter(Boolean).join("\n\n")});
      } else if (tag === "system-reminder" || tag === "pasted_content") {
        parts.push({kind:"context", title:tag === "system-reminder" ? "Session Note" : "Pasted Text", parts:parse(body, depth + 1,streaming)});
      } else if (tag === "INSTRUCTIONS") {
        // The desktop prepends this heading to its project instruction envelope.
        if (parts.at(-1)?.kind === "text" && /^# AGENTS\.md instructions\s*$/.test(parts.at(-1).text)) parts.pop();
        parts.push({kind:"context", title:"Project Instructions", text:body});
      } else if (tag === "local-command-caveat") {
        // Provider guidance for interpreting the following local command.
      } else {
        const labels = {"command-name":"Command", "command-args":"Arguments", "command-message":"Command", "local-command-stdout":"Command Output"};
        parts.push({kind:"context", title:labels[tag], text:body});
      }
      const remainder = block.slice(at + close.length).trim();
      if (remainder) lines.splice(i + 1, 0, remainder);
    }
    flush(); return parts;
  }
  function fileCitation(body) {
    const fields = new Map();
    const attribute = /\s*([a-zA-Z_]\w*)\s*=\s*("(?:\\.|[^"\\])*"|'[^']*'|\d+)/y;
    let at = 0;
    while (body.slice(at).trim()) {
      attribute.lastIndex = at;
      const match = attribute.exec(body);
      if (!match || fields.has(match[1])) return null;
      let value = match[2];
      try { value = value.startsWith('"') ? JSON.parse(value) : value.replace(/^'|'$/g, ''); } catch { return null; }
      fields.set(match[1], value); at = attribute.lastIndex;
    }
    const path = fields.get('path');
    if (typeof path !== 'string' || !/^(\/|[a-z]:[\\/])/i.test(path) || /[\u0000-\u001f]/.test(path)) return null;
    let label = path.split(/[\\/]/).pop() || 'Source File';
    const start = fields.get('line_start') ?? fields.get('start_line') ?? fields.get('line');
    const end = fields.get('line_end') ?? fields.get('end_line');
    if (/^[1-9]\d*$/.test(start || '')) {
      label += ':' + start;
      if (/^[1-9]\d*$/.test(end || '') && Number(end) > Number(start)) label += '-' + end;
    }
    return {path, label};
  }
  function renderFileCitations(text, render) {
    // Inline code is literal. Fenced blocks are separated by the Markdown renderer.
    return text.replace(/(`+)[^\n]*?\1|:{1,2}codex-file-citation\{((?:[^"'{}\n]|"(?:\\.|[^"\\])*"|'[^']*')*)\}/g, (match, code, body) => {
      if (code) return match;
      const citation = fileCitation(body);
      return citation ? render(citation) : 'Source Unavailable';
    });
  }
  return {parse, renderFileCitations, cleanUserMessage};
})();

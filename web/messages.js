"use strict";

// Presentation only. Keep the original transcript and provider input intact.
const MessageContent = (() => {
  const tags = new Set(["send_user_message_question_reply", "environment_context", "external_codex_apps_open_page", "heartbeat", "INSTRUCTIONS", "local-command-caveat", "command-name", "command-args", "command-message", "local-command-stdout", "system-reminder", "task-notification", "pasted_content"]);
  function field(body, name) {
    const match = body.match(new RegExp("<" + name + ">([\\s\\S]*?)</" + name + ">"));
    return match ? match[1].trim() : "";
  }
  function parse(text, depth = 0) {
    if (depth >= 8) return [{kind:"text", text:"Additional Session Context"}];
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
      if (!open || !tags.has(open[1])) { plain.push(line); continue; }
      const tag = open[1], close = "</" + tag + ">";
      let end = i;
      while (end < lines.length && !lines[end].includes(close)) end++;
      if (end === lines.length) { plain.push(line); continue; }
      const block = lines.slice(i, end + 1).join("\n"), at = block.indexOf(close);
      const body = block.slice(block.indexOf(">") + 1, at).trim();
      flush(); i = end;
      if (tag === "send_user_message_question_reply") {
        let replies;
        try { replies = JSON.parse(body); } catch {}
        if (Array.isArray(replies) && replies.length && replies.every(r => r && typeof r.question === "string" && typeof r.answer === "string")) {
          for (const reply of replies) parts.push({kind:"reply", question:reply.question, answer:reply.answer});
        } else parts.push({kind:"text", text:"Answered a question"});
      } else if (tag === "external_codex_apps_open_page") {
        let page;
        try {page = JSON.parse(body);}catch {}
        parts.push({kind:"context", title:page?.page_id === null ? "Page Closed" : page?.page_id ? "Opened a Page" : "Page Context Updated", text:""});
      } else if (tag === "environment_context") {
        const labels = {cwd:"Working Directory", shell:"Shell", current_date:"Date", timezone:"Time Zone"};
        const rows = Object.entries(labels).map(([key,label]) => { const value = field(body,key); return value ? label + ": " + value : ""; }).filter(Boolean);
        parts.push({kind:"context", title:"Session Context", text:rows.join("\n")});
      } else if (tag === "heartbeat") {
        parts.push({kind:"context", title:"Scheduled Check", text:field(body,"instructions")});
      } else if (tag === "task-notification") {
        const status = field(body,"status");
        const title = {completed:"Task Completed", failed:"Task Failed", stopped:"Task Stopped", running:"Task Running"}[status] || "Task Update";
        parts.push({kind:"context", title, text:[field(body,"summary"),field(body,"result")].filter(Boolean).join("\n\n")});
      } else if (tag === "system-reminder" || tag === "pasted_content") {
        parts.push({kind:"context", title:tag === "system-reminder" ? "Session Note" : "Pasted Text", parts:parse(body, depth + 1)});
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
  return {parse};
})();

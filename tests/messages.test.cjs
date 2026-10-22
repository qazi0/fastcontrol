const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const context = vm.createContext({});
vm.runInContext(fs.readFileSync('web/messages.js', 'utf8') + '\nthis.parse = MessageContent.parse;', context);
const parse = text => JSON.parse(JSON.stringify(context.parse(text)));

test('question replies expose questions and answers without transport identifiers', () => {
  const replies = [{questionItemId:'private-call-id',question:'Which folder?',answer:'My reports'}, {question:'Continue?',answer:'Yes'}];
  assert.deepEqual(parse('<send_user_message_question_reply>\n' + JSON.stringify(replies) + '\n</send_user_message_question_reply>'), [
    {kind:'reply',question:'Which folder?',answer:'My reports'}, {kind:'reply',question:'Continue?',answer:'Yes'}
  ]);
});
test('injected instructions and session metadata become named context', () => {
  assert.deepEqual(parse('# AGENTS.md instructions\n\n<INSTRUCTIONS>\nUse clear names.\n</INSTRUCTIONS><environment_context>\n<cwd>/work</cwd>\n<shell>zsh</shell>\n<filesystem>private internals</filesystem>\n</environment_context>\nFix the bug.'), [
    {kind:'context',title:'Project Instructions',text:'Use clear names.'},
    {kind:'context',title:'Session Context',text:'Working Directory: /work\nShell: zsh'},
    {kind:'text',text:'Fix the bug.'}
  ]);
});
test('ordinary XML, inline examples, fenced examples and incomplete envelopes remain text', () => {
  for (const text of ['<invoice>hello</invoice>', 'Example: <send_user_message_question_reply>[]</send_user_message_question_reply>', '```xml\n<send_user_message_question_reply>[]</send_user_message_question_reply>\n```', '~~~\n<environment_context>x</environment_context>\n~~~', '<environment_context>still typing']) assert.deepEqual(parse(text), [{kind:'text',text}]);
});
test('bad reply payloads never expose raw control JSON', () => {
  for (const body of ['{broken', 'null', '{}', '[{"questionItemId":"private"}]']) assert.deepEqual(parse('<send_user_message_question_reply>' + body + '</send_user_message_question_reply>'), [{kind:'text',text:'Answered a question'}]);
});
test('scheduled checks and local commands hide protocol scaffolding', () => {
  assert.deepEqual(parse('<heartbeat><automation_id>private</automation_id><instructions>Check the build.</instructions></heartbeat>\n<command-name>/status</command-name>'), [
    {kind:'context',title:'Scheduled Check',text:'Check the build.'}, {kind:'context',title:'Command',text:'/status'}
  ]);
});
test('Claude task notices show the result and omit internal paths and IDs', () => {
  const result = parse('<system-reminder>\n<task-notification><task-id>private</task-id><output-file>/private/output</output-file><status>completed</status><summary>Checked the files</summary><result>Everything passed.</result></task-notification>\n</system-reminder>');
  assert.deepEqual(result, [{kind:'context',title:'Session Note',parts:[{kind:'context',title:'Task Completed',text:'Checked the files\n\nEverything passed.'}]}]);
});

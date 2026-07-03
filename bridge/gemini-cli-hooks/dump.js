const fs = require('fs');
const data = fs.readFileSync(0, 'utf8');
fs.appendFileSync('hook_dump.log', `--- ${process.argv[2]} ---\n${data}\n\n`);
console.log(JSON.stringify({ decision: 'allow' }));

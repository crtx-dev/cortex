const fs = require('fs');
const path = require('path');

// Standardized account-creation contract for Cortex. Every surface that can
// create a new local account must present Username, Email, Password and
// Confirm password, in that order, and never a display-name field.
function assert(cond, msg) { if (!cond) throw new Error(msg); }

// 1. First-run setup (served + Nift source).
for (const file of ['public/index.html', 'content/index.html']) {
  const html = fs.readFileSync(file, 'utf8');
  const setup = html.split('<form id="setupForm"')[1].split('</form>')[0];
  const ids = [...setup.matchAll(/id="([A-Za-z-]+)"/g)].map(m => m[1]);
  for (const id of ['setupUsername', 'setupEmail', 'setupPassword', 'setupConfirm']) {
    assert(ids.includes(id), file + ' setup form missing ' + id);
  }
  assert(!ids.includes('setupDisplay'), file + ' setup form still requests a display name');
}
assert(fs.readFileSync('public/index.html', 'utf8').includes('id="loginUsername"'), 'login must accept username or email');

// 2. /manage Add user (dialog + payload).
const manageHtml = fs.readFileSync('public/manage.html', 'utf8');
const dialog = manageHtml.split('<dialog>')[1].split('</dialog>')[0];
for (const id of ['username', 'email', 'password', 'confirm']) {
  assert(dialog.includes('name="' + id + '"'), 'manage Add user dialog missing ' + id);
}
assert(!/Display name|display/.test(dialog), 'manage Add user dialog still requests a display name');

const manageJs = fs.readFileSync('public/assets/js/manage.js', 'utf8');
assert(manageJs.includes("'Passwords do not match.'") || manageJs.includes('"Passwords do not match."'), 'manage frontend must validate password confirmation');
assert(!manageJs.includes("display:f.get"), 'manage create-account payload must not send a user-supplied display name');
assert(manageJs.includes('username:f.get("username")') && manageJs.includes('email:f.get("email")'), 'manage create-account must send username and email');

const appJs = fs.readFileSync('public/assets/js/script.js', 'utf8');
assert(appJs.includes('Passwords do not match.'), 'setup frontend must validate password confirmation');

console.log('cortex account-creation contract: ok');
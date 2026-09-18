const fs = require('fs');

// Standardized account-creation contract for Cortex: setup requires Username,
// Email, Password and Confirm password and never a display name. Login remains
// username-or-email.
const html = fs.readFileSync('public/index.html', 'utf8');
const setup = html.split('<form id="setupForm"')[1].split('</form>')[0];
const ids = [...setup.matchAll(/id="([A-Za-z-]+)"/g)].map(m => m[1]);
for (const id of ['setupUsername', 'setupEmail', 'setupPassword', 'setupConfirm']) {
  if (!ids.includes(id)) throw new Error('setup form missing ' + id);
}
if (ids.includes('setupDisplay')) throw new Error('setup form still requests a display name');
if (!html.includes('id="loginUsername"') || !html.includes('Username or email')) {
  throw new Error('login form must accept username or email');
}
const js = fs.readFileSync('public/assets/js/script.js', 'utf8');
if (!js.includes("'Passwords do not match.'")) {
  throw new Error('setup frontend must validate password confirmation');
}
console.log('cortex account-creation contract: ok');
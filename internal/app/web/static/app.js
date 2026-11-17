// Progressive enhancement: nothing here is needed for any flow to work.
// After a form is submitted, disable its buttons so a second tap cannot send
// it twice. The disabling waits one tick so the clicked button's name and
// value are still part of the submission.
document.addEventListener('submit', function (event) {
  var form = event.target;
  if (!(form instanceof HTMLFormElement) || event.defaultPrevented) return;
  // A slow action (a book lookup can take several seconds) says it is working,
  // so a tap on a phone never looks like it did nothing.
  var sender = event.submitter;
  if (sender && sender.hasAttribute('data-busy-label')) {
    sender.setAttribute('data-idle-label', sender.textContent);
    sender.textContent = sender.getAttribute('data-busy-label');
    form.setAttribute('aria-busy', 'true');
    var status = form.querySelector('[data-busy-status]');
    if (status) status.hidden = false;
  }
  setTimeout(function () {
    form.querySelectorAll('button:not([disabled])').forEach(function (b) {
      b.disabled = true;
      b.setAttribute('data-was-submitting', '');
    });
  }, 0);
});
// Coming back with the browser's Back button restores the page from memory;
// give the buttons back.
window.addEventListener('pageshow', function () {
  document.querySelectorAll('button[data-was-submitting]').forEach(function (b) {
    b.disabled = false;
    b.removeAttribute('data-was-submitting');
    if (b.hasAttribute('data-idle-label')) {
      b.textContent = b.getAttribute('data-idle-label');
      b.removeAttribute('data-idle-label');
    }
  });
  document.querySelectorAll('form[aria-busy]').forEach(function (f) {
    f.removeAttribute('aria-busy');
    var status = f.querySelector('[data-busy-status]');
    if (status) status.hidden = true;
  });
});

// Open the Preferences section a link points at (#privacy or #ai); sections
// are collapsed otherwise. Without script they open from the Preferences page.
function openSettingsSection() {
  try {
    var target = location.hash && document.querySelector('details.settings-section' + location.hash);
    if (target) target.open = true;
  } catch (e) { /* a hash that is not a valid selector */ }
}
window.addEventListener('hashchange', openSettingsSection);
openSettingsSection();

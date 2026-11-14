// Progressive enhancement: nothing here is needed for any flow to work.
// After a form is submitted, disable its buttons so a second tap cannot send
// it twice. The disabling waits one tick so the clicked button's name and
// value are still part of the submission.
document.addEventListener('submit', function (event) {
  var form = event.target;
  if (!(form instanceof HTMLFormElement) || event.defaultPrevented) return;
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
  });
});

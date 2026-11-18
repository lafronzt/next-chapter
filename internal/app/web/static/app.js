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

// A Hardcover cover that fails to load is removed, which shows the title tile
// beneath it. Error events do not bubble, so listen while they are captured.
document.addEventListener('error', function (e) {
  var t = e.target;
  if (t && t.tagName === 'IMG' && t.classList.contains('hc-cover')) t.remove();
}, true);


// Keep the selected library view visible inside the horizontally scrolling tabs.
var libraryTabs = document.querySelector('.tabs');
var selectedTab = libraryTabs && libraryTabs.querySelector('.selected');
if (selectedTab) {
  var selectedLeft = selectedTab.getBoundingClientRect().left - libraryTabs.getBoundingClientRect().left;
  libraryTabs.scrollLeft += selectedLeft - (libraryTabs.clientWidth - selectedTab.offsetWidth) / 2;
}

// Disclosure menus remain usable without JavaScript; enhance dismissal for
// keyboard users and when the reader taps elsewhere or opens an outbound link.
var menuSelector = 'details.book-links, details.tab-more, details.row-more';
document.addEventListener('click', function (event) {
  if (!(event.target instanceof Element)) return;
  document.querySelectorAll(menuSelector).forEach(function (menu) {
    if (menu.open && (!menu.contains(event.target) || event.target.closest('a'))) menu.open = false;
  });
});
document.addEventListener('keydown', function (event) {
  if (event.key !== 'Escape') return;
  document.querySelectorAll(menuSelector).forEach(function (menu) {
    if (!menu.open) return;
    var containedFocus = menu.contains(document.activeElement);
    menu.open = false;
    if (containedFocus) menu.querySelector('summary').focus();
  });
});


// Labels, examples and the keypad follow the selected method. Keep drafts
// separate so switching units never silently reinterprets the previous value.
document.querySelectorAll('.reading-progress').forEach(function (form) {
  var unit = form.querySelector('select[name=unit]');
  var input = form.querySelector('input[name=progress]');
  if (!unit || !input) return;
  var previous = unit.value;
  var drafts = {};
  function updateProgressFields() {
    var label = form.querySelector('[data-progress-label]');
    var method = form.querySelector('[data-progress-method]');
    if (label) label.textContent = unit.value === 'seconds' ? 'Audio position' : unit.value === 'percent' ? 'Progress (%)' : 'Page reached';
    if (method) method.textContent = unit.value === 'seconds' ? 'Audio position' : unit.value === 'percent' ? 'Percentage' : 'Page';
    input.inputMode = unit.value === 'seconds' ? 'text' : 'numeric';
    input.placeholder = unit.value === 'seconds' ? 'e.g. 2:30' : unit.value === 'percent' ? 'e.g. 35' : 'e.g. 120';
  }
  unit.addEventListener('change', function () {
    drafts[previous] = input.value;
    input.value = drafts[unit.value] || '';
    previous = unit.value;
    updateProgressFields();
  });
  updateProgressFields();
});

// The native select is the submitted value and remains usable without script.
// Stars are shortcuts; exact quarter ratings are never rounded on load/save.
document.querySelectorAll('.rating-input').forEach(function (control) {
  var select = control.querySelector('select[name=rating]');
  var stars = control.querySelector('.rating-stars');
  var feedback = control.querySelector('.rating-feedback');
  var output = control.querySelector('.rating-value');
  var clear = control.querySelector('[data-clear-rating]');
  var precision = control.querySelector('.rating-precision');
  if (!select || !stars || !feedback) return;
  function updateRating() {
    var value = select.value === '' ? 0 : Number(select.value);
    output.textContent = value ? value + ' / 5' : 'Unrated';
    clear.disabled = value === 0;
    stars.querySelectorAll('[data-rating-value]').forEach(function (button) {
      var n = Number(button.dataset.ratingValue);
      var fraction = Math.max(0, Math.min(1, value - n + 1));
      button.dataset.fill = ['empty', 'quarter', 'half', 'three-quarters', 'full'][Math.round(fraction * 4)];
      button.setAttribute('aria-pressed', String(value === n));
    });
  }
  stars.addEventListener('click', function (event) {
    var button = event.target.closest('[data-rating-value]');
    if (!button) return;
    select.value = button.dataset.ratingValue;
    select.dispatchEvent(new Event('change', { bubbles:true }));
  });
  clear.addEventListener('click', function () {
    select.value = '';
    select.dispatchEvent(new Event('change', { bubbles:true }));
  });
  select.addEventListener('change', updateRating);
  stars.hidden = false;
  feedback.hidden = false;
  precision.open = false;
  updateRating();
});

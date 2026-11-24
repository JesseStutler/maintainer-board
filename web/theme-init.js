// Applies the saved theme before first paint. Loaded as a classic script so
// it runs ahead of the stylesheet-dependent render.
(function () {
  var theme = null;
  try {
    theme = localStorage.getItem('mb.theme');
  } catch (err) {
    theme = null;
  }
  if (theme !== 'light' && theme !== 'dark') {
    theme = 'dark';
  }
  document.documentElement.setAttribute('data-theme', theme);
})();

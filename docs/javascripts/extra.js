document.addEventListener("DOMContentLoaded", function () {
  var blocks = document.querySelectorAll(".md-typeset pre");
  blocks.forEach(function (pre) {
    var code = pre.querySelector("code");
    if (!code || code.scrollWidth <= pre.clientWidth) return;
    var container = pre.parentElement;
    var copyBtn = container.querySelector(
      '.md-code__button[data-md-type="copy"]'
    );
    var btn = document.createElement("button");
    btn.className = "md-code__button md-icon";
    btn.type = "button";
    btn.dataset.mdType = "expand";
    btn.title = "Expand code";
    if (copyBtn) {
      copyBtn.after(btn);
    } else {
      container.insertBefore(btn, pre);
    }
    btn.addEventListener("click", function () {
      var expanded = pre.classList.toggle("md-code--expanded");
      btn.dataset.mdType = expanded ? "collapse" : "expand";
      var label = expanded ? "Collapse code" : "Expand code";
      btn.title = label;
    });
  });
});

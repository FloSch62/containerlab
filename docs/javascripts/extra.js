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

    var tooltip = document.createElement("div");
    tooltip.className = "md-tooltip2 md-tooltip2--bottom";
    tooltip.setAttribute("role", "tooltip");
    var inner = document.createElement("div");
    inner.className = "md-tooltip2__inner";
    inner.textContent = btn.title;
    tooltip.appendChild(inner);
    document.body.appendChild(tooltip);

    function positionTooltip() {
      var rect = btn.getBoundingClientRect();
      tooltip.style.setProperty(
        "--md-tooltip-host-x",
        window.scrollX + rect.left + rect.width / 2 + "px"
      );
      tooltip.style.setProperty(
        "--md-tooltip-host-y",
        window.scrollY + rect.top + "px"
      );
      tooltip.style.setProperty("--md-tooltip-x", "0px");
      tooltip.style.setProperty(
        "--md-tooltip-y",
        8 + rect.height + "px"
      );
      var w = inner.offsetWidth;
      tooltip.style.setProperty("--md-tooltip-width", w + "px");
      tooltip.style.setProperty("--md-tooltip-tail", "0px");
    }

    function showTooltip() {
      positionTooltip();
      tooltip.classList.add("md-tooltip2--active");
    }

    function hideTooltip() {
      tooltip.classList.remove("md-tooltip2--active");
    }

    btn.addEventListener("mouseenter", showTooltip);
    btn.addEventListener("focus", showTooltip);
    btn.addEventListener("mouseleave", hideTooltip);
    btn.addEventListener("blur", hideTooltip);

    btn.addEventListener("click", function () {
      var expanded = pre.classList.toggle("md-code--expanded");
      btn.dataset.mdType = expanded ? "collapse" : "expand";
      var label = expanded ? "Collapse code" : "Expand code";
      btn.title = label;
      inner.textContent = label;
      showTooltip();
    });
  });
});

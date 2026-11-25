// Inline stroke icons (24x24 grid, Lucide-style geometry). Each entry lists
// the SVG child elements as [tag, attributes].

const P = (d) => ['path', { d }];
const C = (cx, cy, r) => ['circle', { cx, cy, r }];
const R = (x, y, width, height, rx) => ['rect', { x, y, width, height, rx }];

const ICONS = {
  plus: [P('M5 12h14'), P('M12 5v14')],
  x: [P('M18 6 6 18'), P('m6 6 12 12')],
  search: [C(11, 11, 8), P('m21 21-4.3-4.3')],
  refresh: [
    P('M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8'),
    P('M21 3v5h-5'),
    P('M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16'),
    P('M8 16H3v5'),
  ],
  columns: [R(3, 3, 18, 18, 2), P('M9 3v18'), P('M15 3v18')],
  kanban: [P('M6 5v11'), P('M12 5v6'), P('M18 5v14')],
  history: [P('M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8'), P('M3 3v5h5'), P('M12 7v5l4 2')],
  sun: [
    C(12, 12, 4),
    P('M12 2v2'),
    P('M12 20v2'),
    P('m4.93 4.93 1.41 1.41'),
    P('m17.66 17.66 1.41 1.41'),
    P('M2 12h2'),
    P('M20 12h2'),
    P('m6.34 17.66-1.41 1.41'),
    P('m19.07 4.93-1.41 1.41'),
  ],
  moon: [P('M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z')],
  bell: [P('M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9'), P('M10.3 21a1.94 1.94 0 0 0 3.4 0')],
  'bell-off': [
    P('M8.7 3A6 6 0 0 1 18 8a21.3 21.3 0 0 0 .6 5'),
    P('M17 17H3s3-2 3-9a4.67 4.67 0 0 1 .3-1.7'),
    P('M10.3 21a1.94 1.94 0 0 0 3.4 0'),
    P('m2 2 20 20'),
  ],
  trash: [
    P('M3 6h18'),
    P('M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6'),
    P('M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2'),
    P('M10 11v6'),
    P('M14 11v6'),
  ],
  external: [P('M15 3h6v6'), P('M10 14 21 3'), P('M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6')],
  pr: [C(18, 18, 3), C(6, 6, 3), P('M13 6h3a2 2 0 0 1 2 2v7'), P('M6 9v12')],
  issue: [C(12, 12, 10), C(12, 12, 1)],
  merge: [C(18, 18, 3), C(6, 6, 3), P('M6 21V9a9 9 0 0 0 9 9')],
  commit: [C(12, 12, 3), P('M3 12h6'), P('M15 12h6')],
  message: [P('M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z')],
  check: [P('M20 6 9 17l-5-5')],
  'check-circle': [C(12, 12, 10), P('m9 12 2 2 4-4')],
  'x-circle': [C(12, 12, 10), P('m15 9-6 6'), P('m9 9 6 6')],
  clock: [C(12, 12, 10), P('M12 6v6l4 2')],
  alert: [
    P('m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z'),
    P('M12 9v4'),
    P('M12 17h.01'),
  ],
  info: [C(12, 12, 10), P('M12 16v-4'), P('M12 8h.01')],
  'chevron-left': [P('m15 18-6-6 6-6')],
  'chevron-right': [P('m9 18 6-6-6-6')],
  'chevron-down': [P('m6 9 6 6 6-6')],
  'chevron-up': [P('m18 15-6-6-6 6')],
  'arrow-right': [P('M5 12h14'), P('m12 5 7 7-7 7')],
  'arrow-down': [P('M12 5v14'), P('m19 12-7 7-7-7')],
  send: [P('m22 2-7 20-4-9-9-4Z'), P('M22 2 11 13')],
  stop: [R(6, 6, 12, 12, 1.5)],
  play: [['polygon', { points: '6 3 20 12 6 21 6 3' }]],
  sparkles: [
    P('M9.937 15.5A2 2 0 0 0 8.5 14.063l-6.135-1.582a.5.5 0 0 1 0-.962L8.5 9.936A2 2 0 0 0 9.937 8.5l1.582-6.135a.5.5 0 0 1 .963 0L14.063 8.5A2 2 0 0 0 15.5 9.937l6.135 1.581a.5.5 0 0 1 0 .964L15.5 14.063a2 2 0 0 0-1.437 1.437l-1.582 6.135a.5.5 0 0 1-.963 0z'),
    P('M20 3v4'),
    P('M22 5h-4'),
  ],
  link: [
    P('M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71'),
    P('M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71'),
  ],
  unlink: [
    P('m18.84 12.25 1.72-1.71h-.02a5.004 5.004 0 0 0-.12-7.07 5.006 5.006 0 0 0-6.95 0l-1.72 1.71'),
    P('m5.17 11.75-1.71 1.71a5.004 5.004 0 0 0 .12 7.07 5.006 5.006 0 0 0 6.95 0l1.71-1.71'),
    P('M8 2v3'),
    P('M2 8h3'),
    P('M16 19v3'),
    P('M19 16h3'),
  ],
  pencil: [P('M17 3a2.85 2.83 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5Z'), P('m15 5 4 4')],
  copy: [R(8, 8, 14, 14, 2), P('M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2')],
  bot: [P('M12 8V4H8'), R(4, 8, 16, 12, 2), P('M2 14h2'), P('M20 14h2'), P('M15 13v2'), P('M9 13v2')],
  user: [P('M19 21v-2a4 4 0 0 0-4-4H9a4 4 0 0 0-4 4v2'), C(12, 7, 4)],
  branch: [P('M6 3v12'), C(18, 6, 3), C(6, 18, 3), P('M18 9a9 9 0 0 1-9 9')],
  loader: [P('M21 12a9 9 0 1 1-6.219-8.56')],
  power: [P('M12 2v10'), P('M18.4 6.6a9 9 0 1 1-12.77.04')],
  terminal: [['polyline', { points: '4 17 10 11 4 5' }], P('M12 19h8')],
  file: [
    P('M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z'),
    P('M14 2v4a2 2 0 0 0 2 2h4'),
  ],
  zap: [
    P('M4 14a1 1 0 0 1-.78-1.63l9.9-10.2a.5.5 0 0 1 .86.46l-1.92 6.02A1 1 0 0 0 13 10h7a1 1 0 0 1 .78 1.63l-9.9 10.2a.5.5 0 0 1-.86-.46l1.92-6.02A1 1 0 0 0 11 14z'),
  ],
  panel: [R(3, 3, 18, 18, 2), P('M3 9h18')],
  'panel-left': [R(3, 3, 18, 18, 2), P('M9 3v18')],
  'panel-right': [R(3, 3, 18, 18, 2), P('M15 3v18')],
  maximize: [P('M15 3h6v6'), P('m21 3-7 7'), P('m3 21 7-7'), P('M9 21H3v-6')],
  minimize: [P('m14 10 7-7'), P('M20 10h-6V4'), P('m3 21 7-7'), P('M4 14h6v6')],
  'arrow-left': [P('m12 19-7-7 7-7'), P('M19 12H5')],
  eye: [P('M2.06 12.35a1 1 0 0 1 0-.7 10.75 10.75 0 0 1 19.88 0 1 1 0 0 1 0 .7 10.75 10.75 0 0 1-19.88 0'), C(12, 12, 3)],
  pin: [P('M12 17v5'), P('M9 10.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V7a1 1 0 0 1 1-1 2 2 0 0 0 0-4H8a2 2 0 0 0 0 4 1 1 0 0 1 1 1z')],
  more: [C(5, 12, 1.5), C(12, 12, 1.5), C(19, 12, 1.5)],
  sort: [P('m3 16 4 4 4-4'), P('M7 20V4'), P('m21 8-4-4-4 4'), P('M17 4v16')],
};

// Filled brand marks of the agent harnesses, drawn in the harness accent
// color. The Copilot mark is the "copilot" icon of GitHub Octicons (MIT
// license); the Codex mark is the OpenAI logo as published by Simple Icons
// (CC0). Both names and marks belong to their owners and are used here only
// to label sessions of those products.
const BRANDS = {
  copilot: {
    viewBox: '0 0 16 16',
    paths: [
      'M7.998 15.035c-4.562 0-7.873-2.914-7.998-3.749V9.338c.085-.628.677-1.686 1.588-2.065.013-.07.024-.143.036-.218.029-.183.06-.384.126-.612-.201-.508-.254-1.084-.254-1.656 0-.87.128-1.769.693-2.484.579-.733 1.494-1.124 2.724-1.261 1.206-.134 2.262.034 2.944.765.05.053.096.108.139.165.044-.057.094-.112.143-.165.682-.731 1.738-.899 2.944-.765 1.23.137 2.145.528 2.724 1.261.566.715.693 1.614.693 2.484 0 .572-.053 1.148-.254 1.656.066.228.098.429.126.612.012.076.024.148.037.218.924.385 1.522 1.471 1.591 2.095v1.872c0 .766-3.351 3.795-8.002 3.795Zm0-1.485c2.28 0 4.584-1.11 5.002-1.433V7.862l-.023-.116c-.49.21-1.075.291-1.727.291-1.146 0-2.059-.327-2.71-.991A3.222 3.222 0 0 1 8 6.303a3.24 3.24 0 0 1-.544.743c-.65.664-1.563.991-2.71.991-.652 0-1.236-.081-1.727-.291l-.023.116v4.255c.419.323 2.722 1.433 5.002 1.433ZM6.762 2.83c-.193-.206-.637-.413-1.682-.297-1.019.113-1.479.404-1.713.7-.247.312-.369.789-.369 1.554 0 .793.129 1.171.308 1.371.162.181.519.379 1.442.379.853 0 1.339-.235 1.638-.54.315-.322.527-.827.617-1.553.117-.935-.037-1.395-.241-1.614Zm4.155-.297c-1.044-.116-1.488.091-1.681.297-.204.219-.359.679-.242 1.614.091.726.303 1.231.618 1.553.299.305.784.54 1.638.54.922 0 1.28-.198 1.442-.379.179-.2.308-.578.308-1.371 0-.765-.123-1.242-.37-1.554-.233-.296-.693-.587-1.713-.7Z',
      'M6.25 9.037a.75.75 0 0 1 .75.75v1.501a.75.75 0 0 1-1.5 0V9.787a.75.75 0 0 1 .75-.75Zm4.25.75v1.501a.75.75 0 0 1-1.5 0V9.787a.75.75 0 0 1 1.5 0Z',
    ],
  },
  codex: {
    viewBox: '0 0 24 24',
    paths: [
      'M22.2819 9.8211a5.9847 5.9847 0 0 0-.5157-4.9108 6.0462 6.0462 0 0 0-6.5098-2.9A6.0651 6.0651 0 0 0 4.9807 4.1818a5.9847 5.9847 0 0 0-3.9977 2.9 6.0462 6.0462 0 0 0 .7427 7.0966 5.98 5.98 0 0 0 .511 4.9107 6.051 6.051 0 0 0 6.5146 2.9001A5.9847 5.9847 0 0 0 13.2599 24a6.0557 6.0557 0 0 0 5.7718-4.2058 5.9894 5.9894 0 0 0 3.9977-2.9001 6.0557 6.0557 0 0 0-.7475-7.0729zm-9.022 12.6081a4.4755 4.4755 0 0 1-2.8764-1.0408l.1419-.0804 4.7783-2.7582a.7948.7948 0 0 0 .3927-.6813v-6.7369l2.02 1.1686a.071.071 0 0 1 .038.052v5.5826a4.504 4.504 0 0 1-4.4945 4.4944zm-9.6607-4.1254a4.4708 4.4708 0 0 1-.5346-3.0137l.142.0852 4.783 2.7582a.7712.7712 0 0 0 .7806 0l5.8428-3.3685v2.3324a.0804.0804 0 0 1-.0332.0615L9.74 19.9502a4.4992 4.4992 0 0 1-6.1408-1.6464zM2.3408 7.8956a4.485 4.485 0 0 1 2.3655-1.9728V11.6a.7664.7664 0 0 0 .3879.6765l5.8144 3.3543-2.0201 1.1685a.0757.0757 0 0 1-.071 0l-4.8303-2.7865A4.504 4.504 0 0 1 2.3408 7.872zm16.5963 3.8558L13.1038 8.364 15.1192 7.2a.0757.0757 0 0 1 .071 0l4.8303 2.7913a4.4944 4.4944 0 0 1-.6765 8.1042v-5.6772a.79.79 0 0 0-.407-.667zm2.0107-3.0231l-.142-.0852-4.7735-2.7818a.7759.7759 0 0 0-.7854 0L9.409 9.2297V6.8974a.0662.0662 0 0 1 .0284-.0615l4.8303-2.7866a4.4992 4.4992 0 0 1 6.6802 4.66zM8.3065 12.863l-2.02-1.1638a.0804.0804 0 0 1-.038-.0567V6.0742a4.4992 4.4992 0 0 1 7.3757-3.4537l-.142.0805L8.704 5.459a.7948.7948 0 0 0-.3927.6813zm1.0976-2.3654l2.602-1.4998 2.6069 1.4998v2.9994l-2.5974 1.4997-2.6067-1.4997Z',
    ],
  },
};

const SVG_NS = 'http://www.w3.org/2000/svg';

/** Returns a <span class="icon"> wrapping the named SVG icon. */
export function icon(name, size = 14, extraClass = '') {
  const shapes = ICONS[name] || ICONS.info;
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('width', String(size));
  svg.setAttribute('height', String(size));
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '2');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('focusable', 'false');
  for (const [tag, attrs] of shapes) {
    const node = document.createElementNS(SVG_NS, tag);
    for (const [key, value] of Object.entries(attrs)) {
      if (value !== undefined) node.setAttribute(key, String(value));
    }
    svg.appendChild(node);
  }
  const span = document.createElement('span');
  span.className = extraClass ? `icon ${extraClass}` : 'icon';
  span.appendChild(svg);
  return span;
}

/**
 * Returns the brand mark of an agent harness ("copilot" or "codex") as a
 * <span class="icon brand-icon brand-NAME">. Any other harness gets a neutral
 * terminal glyph, so a new agent type still renders something sensible.
 */
export function brandIcon(harness, size = 15) {
  const brand = Object.prototype.hasOwnProperty.call(BRANDS, harness) ? BRANDS[harness] : null;
  if (!brand) {
    const fallback = icon('terminal', size, 'brand-icon brand-other');
    fallback.dataset.brand = 'other';
    return fallback;
  }
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('viewBox', brand.viewBox);
  svg.setAttribute('width', String(size));
  svg.setAttribute('height', String(size));
  svg.setAttribute('fill', 'currentColor');
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('focusable', 'false');
  for (const d of brand.paths) {
    const node = document.createElementNS(SVG_NS, 'path');
    node.setAttribute('d', d);
    svg.appendChild(node);
  }
  const span = document.createElement('span');
  span.className = `icon brand-icon brand-${harness}`;
  span.dataset.brand = harness;
  span.appendChild(svg);
  return span;
}

/** Replaces the content of an existing icon span, e.g. to swap sun and moon. */
export function setIcon(span, name, size = 14) {
  const fresh = icon(name, size);
  span.replaceChildren(...fresh.childNodes);
  span.dataset.icon = name;
}

/** Fills every <span data-icon="name"> placeholder under root. */
export function hydrateIcons(root = document) {
  for (const span of root.querySelectorAll('span[data-icon]')) {
    if (span.firstChild) continue;
    setIcon(span, span.dataset.icon, Number(span.dataset.size) || 14);
  }
}

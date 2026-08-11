import { lang } from './i18n';

// Localized currency names for the tooltip on the Rates card. Instead of a
// hand-maintained table (error-prone, needs a new line per code), names come
// from Intl.DisplayNames — the browser's CLDR data covers every ISO 4217 code
// in both languages, so unknown codes still get a correct official name.
// Falls back to the bare code on exotic runtimes without DisplayNames.

let enNames = null;
let ruNames = null;

function names(locale) {
  try {
    return new Intl.DisplayNames(locale, { type: 'currency' });
  } catch {
    return null;
  }
}

export function currencyName(code) {
  const src = lang.value === 'ru' ? ruNames ||= names('ru') : enNames ||= names('en');
  if (!src) return code;
  try {
    return src.of(code) || code;
  } catch {
    return code;
  }
}
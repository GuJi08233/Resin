import * as countries from "i18n-iso-countries";
import enLocale from "i18n-iso-countries/langs/en.json";
import zhLocale from "i18n-iso-countries/langs/zh.json";
import { getCurrentLocale, isEnglishLocale } from "../../i18n/locale";

countries.registerLocale(enLocale);
countries.registerLocale(zhLocale);

export interface RegionOption {
    code: string;
    name: string;
}

export type ContinentCode = "as" | "eu" | "na" | "sa" | "oc" | "af" | "an";

export interface ContinentOption {
    code: ContinentCode;
    label: string;
}

const CONTINENT_LABELS: Record<ContinentCode, { zh: string; en: string }> = {
    as: { zh: "亚洲", en: "Asia" },
    eu: { zh: "欧洲", en: "Europe" },
    na: { zh: "北美洲", en: "North America" },
    sa: { zh: "南美洲", en: "South America" },
    oc: { zh: "大洋洲", en: "Oceania" },
    af: { zh: "非洲", en: "Africa" },
    an: { zh: "南极洲", en: "Antarctica" },
};

export const CONTINENT_CODES: readonly ContinentCode[] = [
    "as",
    "eu",
    "na",
    "sa",
    "oc",
    "af",
    "an",
];

function getCountryLocale(): "zh" | "en" {
    return isEnglishLocale(getCurrentLocale()) ? "en" : "zh";
}

export const getAllContinents = (): ContinentOption[] => {
    const locale = getCountryLocale();
    return CONTINENT_CODES.map((code) => ({
        code,
        label: CONTINENT_LABELS[code][locale],
    }));
};

export const getContinentLabel = (code: ContinentCode): string => {
    return CONTINENT_LABELS[code][getCountryLocale()];
};

export const getAllRegions = (): RegionOption[] => {
    const names = countries.getNames(getCountryLocale(), { select: "official" });
    return Object.entries(names).map(([code, name]) => ({
        code,
        name: `${code} (${name})`,
    })).sort((a, b) => a.code.localeCompare(b.code));
};

export const getRegionName = (code: string): string | undefined => {
    return countries.getName(code, getCountryLocale(), { select: "official" });
};

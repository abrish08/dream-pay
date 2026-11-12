package transport

import (
	"net/http"
	"strings"
)

// texts: the hosted checkout in English and Amharic. The language comes from
// ?lang=en|am (remembered in a cookie), else the browser's Accept-Language.
type texts struct {
	Lang, Pay, Reference, Choose, NoMethods, PhoneLabel, CheckPhone, ApprovePIN, Waiting, Confirming string
	BankTitle, TransferExactly, WriteReference, AccountNumber, AccountName, BankConfirm              string
	Success, Failed, Return, BadPhone, StartFailed                                                   string
	methods                                                                                          map[string]string
	failures                                                                                         map[string]string
}

var english = texts{
	Lang: "en", Pay: "Pay", Reference: "Reference", Choose: "Choose how to pay",
	NoMethods:  "No payment method is available right now. Please try again shortly.",
	PhoneLabel: "Mobile number of the wallet that pays", CheckPhone: "Check your phone",
	ApprovePIN: "A payment request was sent to your phone. Open it and enter your PIN to approve.",
	Waiting:    "Waiting for confirmation… this page updates by itself.", Confirming: "Confirming your payment",
	BankTitle: "Pay by bank transfer", TransferExactly: "Transfer exactly", WriteReference: "Write this reference on the transfer",
	AccountNumber: "Account number", AccountName: "Account name",
	BankConfirm: "Your payment is confirmed when the money arrives in our account (usually the same working day). Keep your bank receipt.",
	Success:     "Payment successful", Failed: "Payment not completed", Return: "Return to",
	BadPhone:    "Enter an Ethiopian mobile number, e.g. 0911 234 567.",
	StartFailed: "This payment method could not be started. Please choose another or try again.",
	methods: map[string]string{"telebirr": "telebirr", "cbe_birr": "CBE Birr", "mpesa": "M-Pesa",
		"card": "Card (Visa, Mastercard)", "bank_transfer": "Bank transfer"},
	failures: map[string]string{
		"insufficient_funds":       "There is not enough money in the account. Top up or use another account.",
		"declined_by_customer":     "The payment request was declined on the phone.",
		"customer_did_not_respond": "The payment request was not approved in time.",
		"declined":                 "Your bank or wallet declined the payment.",
		"expired_card":             "The card has expired. Use another card.",
		"authentication_failed":    "The card could not be verified (wrong code). Try again.",
		"expired":                  "The time to pay has run out.",
		"risk_declined":            "This payment cannot be made. Please contact the merchant.",
		"":                         "The payment could not be completed. No money was taken. Please try again.",
	},
}

// Amharic: reviewed wording should be confirmed by a native speaker before launch.
var amharic = texts{
	Lang: "am", Pay: "ይክፈሉ", Reference: "መለያ ቁጥር", Choose: "የክፍያ መንገድ ይምረጡ",
	NoMethods:  "በአሁኑ ጊዜ የሚገኝ የክፍያ መንገድ የለም። እባክዎ ትንሽ ቆይተው ይሞክሩ።",
	PhoneLabel: "የሚከፍሉበት የሞባይል ቁጥር", CheckPhone: "ስልክዎን ይመልከቱ",
	ApprovePIN: "የክፍያ ጥያቄ ወደ ስልክዎ ተልኳል። ጥያቄውን ከፍተው ፒንዎን በማስገባት ያረጋግጡ።",
	Waiting:    "ማረጋገጫ በመጠበቅ ላይ… ይህ ገጽ በራሱ ይታደሳል።", Confirming: "ክፍያዎ በመረጋገጥ ላይ ነው",
	BankTitle: "በባንክ ዝውውር ይክፈሉ", TransferExactly: "ይህን መጠን በትክክል ያስተላልፉ", WriteReference: "ይህን መለያ ቁጥር በዝውውሩ ላይ ይጻፉ",
	AccountNumber: "የሂሳብ ቁጥር", AccountName: "የሂሳብ ስም",
	BankConfirm: "ገንዘቡ ወደ ሂሳባችን ሲደርስ ክፍያዎ ይረጋገጣል (ብዙውን ጊዜ በዚያው የሥራ ቀን)። የባንክ ደረሰኝዎን ያስቀምጡ።",
	Success:     "ክፍያው ተሳክቷል", Failed: "ክፍያው አልተጠናቀቀም", Return: "ተመለስ ወደ",
	BadPhone:    "የኢትዮጵያ ሞባይል ቁጥር ያስገቡ፤ ለምሳሌ 0911 234 567።",
	StartFailed: "ይህን የክፍያ መንገድ መጀመር አልተቻለም። እባክዎ ሌላ ይምረጡ ወይም እንደገና ይሞክሩ።",
	methods: map[string]string{"telebirr": "ቴሌብር (telebirr)", "cbe_birr": "ሲቢኢ ብር (CBE Birr)", "mpesa": "ኤም-ፔሳ (M-Pesa)",
		"card": "ካርድ (ቪዛ፣ ማስተርካርድ)", "bank_transfer": "የባንክ ዝውውር"},
	failures: map[string]string{
		"insufficient_funds":       "በሂሳቡ ውስጥ በቂ ገንዘብ የለም። ሂሳብዎን ይሙሉ ወይም ሌላ ሂሳብ ይጠቀሙ።",
		"declined_by_customer":     "የክፍያ ጥያቄው በስልኩ ላይ ውድቅ ተደርጓል።",
		"customer_did_not_respond": "የክፍያ ጥያቄው በጊዜው አልተረጋገጠም።",
		"declined":                 "ባንክዎ ወይም ዋሌትዎ ክፍያውን አልተቀበለውም።",
		"expired_card":             "ካርዱ ጊዜው አልፎበታል። ሌላ ካርድ ይጠቀሙ።",
		"authentication_failed":    "ካርዱ ሊረጋገጥ አልቻለም (የተሳሳተ ኮድ)። እንደገና ይሞክሩ።",
		"expired":                  "የመክፈያ ጊዜው አልፏል።",
		"risk_declined":            "ይህ ክፍያ ሊፈጸም አይችልም። እባክዎ ነጋዴውን ያነጋግሩ።",
		"":                         "ክፍያው ሊጠናቀቅ አልቻለም። ምንም ገንዘብ አልተወሰደም። እባክዎ እንደገና ይሞክሩ።",
	},
}

func lang(w http.ResponseWriter, r *http.Request) texts {
	choice := r.URL.Query().Get("lang")
	if choice == "am" || choice == "en" {
		http.SetCookie(w, &http.Cookie{Name: "lang", Value: choice, Path: "/", MaxAge: 365 * 24 * 3600,
			HttpOnly: true, SameSite: http.SameSiteLaxMode})
	} else if c, err := r.Cookie("lang"); err == nil {
		choice = c.Value
	} else if strings.HasPrefix(strings.ToLower(r.Header.Get("Accept-Language")), "am") {
		choice = "am"
	}
	if choice == "am" {
		return amharic
	}
	return english
}

func (t texts) methodLabel(method, fallback string) string {
	if l, ok := t.methods[method]; ok {
		return l
	}
	return fallback
}

// failure explains why a payment did not complete, in plain words.
func (t texts) failure(status, reason string) string {
	if status == "expired" {
		return t.failures["expired"]
	}
	key, _, _ := strings.Cut(reason, ":")
	if msg, ok := t.failures[key]; ok {
		return msg
	}
	return t.failures[""]
}

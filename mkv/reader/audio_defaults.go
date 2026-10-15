package reader

// encryptionName names a ContentEncryption from its ContentEncAlgo and, for AES, its AESSettingsCipherMode.
func encryptionName(algo, mode uint64) string {
	switch algo {
	case 1:
		return "DES"
	case 2:
		return "3DES"
	case 3:
		return "Twofish"
	case 4:
		return "Blowfish"
	case 5:
		switch mode {
		case 1:
			return "AES-CTR"
		case 2:
			return "AES-CBC"
		}
		return "AES"
	}
	return "unspecified"
}
